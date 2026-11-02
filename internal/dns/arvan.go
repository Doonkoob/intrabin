package dns

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// ArvanAPI is the production ArvanCloud CDN API base URL.
const ArvanAPI = "https://napi.arvancloud.ir/cdn/4.0"

// Arvan implements Provider for ArvanCloud CDN DNS with an API key.
type Arvan struct{ h *httpClient }

// NewArvan returns a client. The key may be given with or without the "Apikey " prefix shown in
// the ArvanCloud panel.
func NewArvan(base, key string) *Arvan {
	if base == "" {
		base = ArvanAPI
	}
	key = strings.TrimSpace(key)
	if !strings.HasPrefix(strings.ToLower(key), "apikey ") {
		key = "Apikey " + key
	}
	return &Arvan{h: newHTTPClient(strings.TrimRight(base, "/"), key)}
}

func (a *Arvan) Kind() string { return "arvancloud" }

type arEnvelope struct {
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
	Errors  any             `json:"errors"`
	Meta    *struct {
		CurrentPage int `json:"current_page"`
		LastPage    int `json:"last_page"`
	} `json:"meta"`
}

func (a *Arvan) call(ctx context.Context, method, path string, body, out any) (*arEnvelope, error) {
	status, b, err := a.h.do(ctx, method, path, body)
	if err != nil {
		return nil, err
	}
	var env arEnvelope
	_ = json.Unmarshal(b, &env)
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return nil, ErrAuth
	case status == http.StatusNotFound:
		return nil, ErrNotFound
	case status == http.StatusTooManyRequests:
		return nil, ErrRateLimited
	case status >= 300:
		msg := env.Message
		if env.Errors != nil {
			eb, _ := json.Marshal(env.Errors)
			msg = strings.TrimSpace(msg + " " + string(eb))
		}
		return nil, &ProviderError{Status: status, Message: truncate(msg, 300)}
	}
	if out != nil && len(env.Data) > 0 {
		if err := json.Unmarshal(env.Data, out); err != nil {
			return nil, &ProviderError{Status: status, Message: "unexpected response format"}
		}
	}
	return &env, nil
}

func (a *Arvan) Verify(ctx context.Context) error {
	_, err := a.call(ctx, http.MethodGet, "/domains?per_page=1&page=1", nil, nil)
	return err
}

func (a *Arvan) Zones(ctx context.Context) ([]Zone, error) {
	var out []Zone
	for page := 1; page <= 200; page++ {
		var ds []struct {
			ID     string `json:"id"`
			Domain string `json:"domain"`
			Name   string `json:"name"`
			Status string `json:"status"`
		}
		env, err := a.call(ctx, http.MethodGet, "/domains?per_page=50&page="+strconv.Itoa(page), nil, &ds)
		if err != nil {
			return nil, err
		}
		for _, d := range ds {
			name := d.Domain
			if name == "" {
				name = d.Name
			}
			// ArvanCloud addresses domains by name in the DNS API, so the zone ID is the name.
			out = append(out, Zone{ID: strings.ToLower(name), Name: strings.ToLower(name), Status: d.Status})
		}
		if env.Meta == nil || page >= env.Meta.LastPage || len(ds) == 0 {
			break
		}
	}
	return out, nil
}

func (a *Arvan) toRecord(raw map[string]any, zone Zone) Record {
	str := func(k string) string { s, _ := raw[k].(string); return s }
	r := Record{ID: str("id"), Type: strings.ToUpper(str("type")), Name: FQDN(str("name"), zone.Name), raw: raw}
	if ttl, ok := raw["ttl"].(float64); ok {
		r.TTL = int(ttl)
	}
	r.Proxied, _ = raw["cloud"].(bool)
	switch v := raw["value"].(type) {
	case []any: // A / AAAA
		var ips []string
		for _, it := range v {
			if m, ok := it.(map[string]any); ok {
				if ip, ok := m["ip"].(string); ok {
					ips = append(ips, ip)
				}
			}
		}
		sort.Strings(ips)
		r.Content = strings.Join(ips, ",")
	case map[string]any:
		for _, k := range []string{"host", "text", "location", "domain", "target"} {
			if s, ok := v[k].(string); ok {
				r.Content = strings.TrimSuffix(s, ".")
				break
			}
		}
	}
	r.Editable = isEditable(r.Type)
	return r
}

func (a *Arvan) Records(ctx context.Context, zone Zone) ([]Record, error) {
	var out []Record
	for page := 1; page <= 200; page++ {
		var rs []map[string]any
		env, err := a.call(ctx, http.MethodGet, "/domains/"+url.PathEscape(zone.Name)+"/dns-records?per_page=100&page="+strconv.Itoa(page), nil, &rs)
		if err != nil {
			return nil, err
		}
		for _, r := range rs {
			out = append(out, a.toRecord(r, zone))
		}
		if env.Meta == nil || page >= env.Meta.LastPage || len(rs) == 0 {
			break
		}
	}
	return out, nil
}

// body builds the request body; for updates it starts from the current raw record so that
// ArvanCloud-specific settings (upstream_https, ip_filter_mode, per-IP port/weight/country) are kept.
func (a *Arvan) body(zone Zone, cur *Record, in RecordInput) (map[string]any, error) {
	b := map[string]any{}
	if cur != nil {
		for k, v := range cur.raw {
			switch k {
			case "id", "created_at", "updated_at", "is_protected", "can_delete":
				continue
			}
			b[k] = v
		}
	}
	b["type"] = strings.ToLower(in.Type)
	b["name"] = Relative(in.Name, zone.Name)
	b["cloud"] = in.Proxied
	ttl := in.TTL
	if ttl == 1 {
		ttl = 120 // ArvanCloud has no "automatic" TTL; 120 s is its panel default
	}
	b["ttl"] = ttl
	switch in.Type {
	case "A", "AAAA":
		prev := map[string]map[string]any{}
		if cur != nil {
			if vs, ok := cur.raw["value"].([]any); ok {
				for _, it := range vs {
					if m, ok := it.(map[string]any); ok {
						if ip, ok := m["ip"].(string); ok {
							prev[ip] = m
						}
					}
				}
			}
		}
		var vals []map[string]any
		for _, ip := range strings.Split(in.Content, ",") {
			if m, ok := prev[ip]; ok {
				vals = append(vals, m)
			} else {
				vals = append(vals, map[string]any{"ip": ip})
			}
		}
		b["value"] = vals
	case "CNAME":
		hh := "source"
		if cur != nil {
			if m, ok := cur.raw["value"].(map[string]any); ok {
				if s, ok := m["host_header"].(string); ok && s != "" {
					hh = s
				}
			}
		}
		b["value"] = map[string]any{"host": in.Content, "host_header": hh}
	case "TXT":
		b["value"] = map[string]any{"text": in.Content}
	default:
		return nil, fmt.Errorf("%w: %s", ErrUnsupported, in.Type)
	}
	return b, nil
}

func (a *Arvan) Create(ctx context.Context, zone Zone, in RecordInput) (*Record, error) {
	b, err := a.body(zone, nil, in)
	if err != nil {
		return nil, err
	}
	var raw map[string]any
	if _, err := a.call(ctx, http.MethodPost, "/domains/"+url.PathEscape(zone.Name)+"/dns-records", b, &raw); err != nil {
		return nil, err
	}
	r := a.toRecord(raw, zone)
	return &r, nil
}

func (a *Arvan) Update(ctx context.Context, zone Zone, cur *Record, in RecordInput) (*Record, error) {
	b, err := a.body(zone, cur, in)
	if err != nil {
		return nil, err
	}
	var raw map[string]any
	if _, err := a.call(ctx, http.MethodPut, "/domains/"+url.PathEscape(zone.Name)+"/dns-records/"+url.PathEscape(cur.ID), b, &raw); err != nil {
		return nil, err
	}
	r := a.toRecord(raw, zone)
	return &r, nil
}

func (a *Arvan) Delete(ctx context.Context, zone Zone, id string) error {
	_, err := a.call(ctx, http.MethodDelete, "/domains/"+url.PathEscape(zone.Name)+"/dns-records/"+url.PathEscape(id), nil, nil)
	return err
}
