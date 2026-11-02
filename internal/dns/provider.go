// Package dns implements DNS provider integrations (Cloudflare, ArvanCloud) and safe record
// changes with snapshots, conflict checks and read-back verification.
//
// Provider APIs used (no undocumented endpoints):
//   - Cloudflare API v4 — https://api.cloudflare.com/client/v4 — GET /user/tokens/verify,
//     GET /zones, GET|POST /zones/{zone_id}/dns_records, PUT|DELETE
//     /zones/{zone_id}/dns_records/{id}; Bearer token; envelope {success, errors, result,
//     result_info}. Checked against Cloudflare's published OpenAPI schema (cloudflare/api-schemas).
//   - ArvanCloud CDN API 4.0 — https://napi.arvancloud.ir/cdn/4.0 — GET /domains,
//     GET|POST /domains/{domain}/dns-records, PUT|DELETE /domains/{domain}/dns-records/{id};
//     "Authorization: Apikey <key>"; envelope {data, meta{current_page,last_page}}; record values
//     are typed objects (A/AAAA: [{ip,…}], CNAME: {host,host_header}, TXT: {text}). Matches the
//     public documentation as implemented by go-acme/lego and dns-lexicon.
package dns

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
)

// Supported record types for changes. Other types are listed read-only.
var EditableTypes = []string{"A", "AAAA", "CNAME", "TXT"}

// Zone is a DNS zone (Cloudflare zone / ArvanCloud domain).
type Zone struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

// Record is a provider-neutral DNS record. Name is always the fully qualified name without a
// trailing dot. Content holds the value: an IP for A/AAAA (several IPs comma-separated for
// ArvanCloud multi-IP records), a hostname for CNAME, the text for TXT.
type Record struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Name     string `json:"name"`
	Content  string `json:"content"`
	TTL      int    `json:"ttl"`
	Proxied  bool   `json:"proxied"`
	Editable bool   `json:"editable"`
	// raw provider representation (used to preserve provider-specific fields on update)
	raw map[string]any
}

// RecordInput is a desired record state.
type RecordInput struct {
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	TTL     int    `json:"ttl"`
	Proxied bool   `json:"proxied"`
}

// Provider is implemented by each DNS provider client.
type Provider interface {
	Kind() string
	Verify(ctx context.Context) error
	Zones(ctx context.Context) ([]Zone, error)
	Records(ctx context.Context, zone Zone) ([]Record, error)
	Create(ctx context.Context, zone Zone, in RecordInput) (*Record, error)
	Update(ctx context.Context, zone Zone, cur *Record, in RecordInput) (*Record, error)
	Delete(ctx context.Context, zone Zone, id string) error
}

var (
	ErrAuth        = errors.New("the provider rejected the credentials")
	ErrNotFound    = errors.New("not found at the provider")
	ErrRateLimited = errors.New("the provider rate-limited the request")
	ErrUnsupported = errors.New("unsupported record")
	ErrInvalid     = errors.New("invalid record")
)

// ProviderError is a non-auth, non-404 error answered by the provider. Message is the provider's
// own error text (it never contains our credentials).
type ProviderError struct {
	Status  int
	Message string
}

func (e *ProviderError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("provider returned HTTP %d", e.Status)
	}
	return fmt.Sprintf("provider returned HTTP %d: %s", e.Status, e.Message)
}

// TransportError means the request may not have reached the provider, or its answer was lost.
type TransportError struct{ Err error }

func (e *TransportError) Error() string { return "could not reach the provider" }
func (e *TransportError) Unwrap() error { return e.Err }

// FQDN normalises a record name: lower case, no trailing dot, "@" or "" means the apex.
func FQDN(name, zone string) string {
	name = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(name)), ".")
	zone = strings.TrimSuffix(strings.ToLower(zone), ".")
	if name == "" || name == "@" {
		return zone
	}
	if name == zone || strings.HasSuffix(name, "."+zone) {
		return name
	}
	return name + "." + zone
}

// Relative returns the name relative to the zone ("@" for the apex).
func Relative(fqdn, zone string) string {
	fqdn, zone = FQDN(fqdn, zone), FQDN("", zone)
	if fqdn == zone {
		return "@"
	}
	return strings.TrimSuffix(fqdn, "."+zone)
}

// Validate normalises and checks a desired record for zone.
func Validate(in *RecordInput, zone string) error {
	in.Type = strings.ToUpper(strings.TrimSpace(in.Type))
	if !slices.Contains(EditableTypes, in.Type) {
		return fmt.Errorf("%w: type must be one of %s", ErrUnsupported, strings.Join(EditableTypes, ", "))
	}
	in.Name = FQDN(in.Name, zone)
	if len(in.Name) > 253 || !validName(strings.TrimSuffix(strings.TrimSuffix(in.Name, FQDN("", zone)), ".")) {
		return fmt.Errorf("%w: invalid name", ErrInvalid)
	}
	in.Content = strings.TrimSpace(in.Content)
	switch in.Type {
	case "A", "AAAA":
		parts := strings.Split(in.Content, ",")
		for i, p := range parts {
			ip, err := netip.ParseAddr(strings.TrimSpace(p))
			if err != nil || (in.Type == "A") != ip.Is4() {
				return fmt.Errorf("%w: %s records need %s addresses", ErrInvalid, in.Type, map[string]string{"A": "IPv4", "AAAA": "IPv6"}[in.Type])
			}
			parts[i] = ip.String()
		}
		in.Content = strings.Join(parts, ",")
	case "CNAME":
		in.Content = strings.TrimSuffix(strings.ToLower(in.Content), ".")
		if !validName(in.Content) || !strings.Contains(in.Content, ".") {
			return fmt.Errorf("%w: CNAME target must be a hostname", ErrInvalid)
		}
	case "TXT":
		if in.Content == "" || len(in.Content) > 2048 {
			return fmt.Errorf("%w: TXT content must be 1–2048 characters", ErrInvalid)
		}
	}
	if in.TTL == 0 {
		in.TTL = 1 // automatic (Cloudflare); mapped per provider
	}
	if in.TTL != 1 && (in.TTL < 30 || in.TTL > 86400) {
		return fmt.Errorf("%w: TTL must be automatic or 30–86400 seconds", ErrInvalid)
	}
	return nil
}

func validName(n string) bool {
	if n == "" {
		return true
	}
	for _, label := range strings.Split(n, ".") {
		if label == "" || len(label) > 63 {
			return false
		}
		for i, c := range label {
			ok := c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_' || (c == '*' && i == 0 && len(label) == 1)
			if !ok {
				return false
			}
		}
	}
	return true
}

// Matches reports whether an existing record already has the desired state (TTL "automatic"
// matches any TTL, because providers report the effective value differently).
func Matches(r *Record, in RecordInput) bool {
	if r == nil {
		return false
	}
	if !strings.EqualFold(r.Type, in.Type) || r.Name != in.Name || !sameContent(r.Type, r.Content, in.Content) || r.Proxied != in.Proxied {
		return false
	}
	return in.TTL == 1 || r.TTL == in.TTL
}

func sameContent(typ, a, b string) bool {
	norm := func(s string) string {
		s = strings.TrimSuffix(strings.TrimSpace(s), ".")
		if typ == "CNAME" {
			s = strings.ToLower(s)
		}
		if typ == "A" || typ == "AAAA" {
			parts := strings.Split(s, ",")
			for i := range parts {
				parts[i] = strings.TrimSpace(parts[i])
			}
			slices.Sort(parts)
			s = strings.Join(parts, ",")
		}
		return s
	}
	return norm(a) == norm(b)
}

// Find returns the record with id, or nil.
func Find(rs []Record, id string) *Record {
	for i := range rs {
		if rs[i].ID == id {
			return &rs[i]
		}
	}
	return nil
}

// FindByName returns the editable records of type at name.
func FindByName(rs []Record, typ, name string) []Record {
	var out []Record
	for _, r := range rs {
		if r.Type == typ && r.Name == name {
			out = append(out, r)
		}
	}
	return out
}
