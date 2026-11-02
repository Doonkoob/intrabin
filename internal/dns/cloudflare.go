package dns

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// CloudflareAPI is the production API base URL.
const CloudflareAPI = "https://api.cloudflare.com/client/v4"

// Cloudflare implements Provider with an API token (Zone:Read + DNS:Edit).
type Cloudflare struct{ h *httpClient }

// NewCloudflare returns a client. base is CloudflareAPI unless a test/mock base is configured.
func NewCloudflare(base, token string) *Cloudflare {
	if base == "" {
		base = CloudflareAPI
	}
	return &Cloudflare{h: newHTTPClient(strings.TrimRight(base, "/"), "Bearer "+token)}
}

func (c *Cloudflare) Kind() string { return "cloudflare" }

type cfEnvelope struct {
	Success bool `json:"success"`
	Errors  []struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"errors"`
	Result     json.RawMessage `json:"result"`
	ResultInfo *struct {
		Page       int `json:"page"`
		TotalPages int `json:"total_pages"`
	} `json:"result_info"`
}

func (c *Cloudflare) call(ctx context.Context, method, path string, body, out any) (*cfEnvelope, error) {
	status, b, err := c.h.do(ctx, method, path, body)
	if err != nil {
		return nil, err
	}
	var env cfEnvelope
	_ = json.Unmarshal(b, &env)
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return nil, ErrAuth
	case status == http.StatusNotFound:
		return nil, ErrNotFound
	case status == http.StatusTooManyRequests:
		return nil, ErrRateLimited
	case status >= 300 || !env.Success:
		msgs := make([]string, 0, len(env.Errors))
		for _, e := range env.Errors {
			msgs = append(msgs, fmt.Sprintf("%s (%d)", e.Message, e.Code))
		}
		return nil, &ProviderError{Status: status, Message: truncate(strings.Join(msgs, "; "), 300)}
	}
	if out != nil && len(env.Result) > 0 {
		if err := json.Unmarshal(env.Result, out); err != nil {
			return nil, &ProviderError{Status: status, Message: "unexpected response format"}
		}
	}
	return &env, nil
}

func (c *Cloudflare) Verify(ctx context.Context) error {
	var r struct {
		Status string `json:"status"`
	}
	if _, err := c.call(ctx, http.MethodGet, "/user/tokens/verify", nil, &r); err != nil {
		return err
	}
	if r.Status != "active" {
		return ErrAuth
	}
	return nil
}

func (c *Cloudflare) Zones(ctx context.Context) ([]Zone, error) {
	var out []Zone
	for page := 1; page <= 200; page++ {
		var zs []struct {
			ID, Name, Status string
		}
		env, err := c.call(ctx, http.MethodGet, "/zones?per_page=50&page="+strconv.Itoa(page), nil, &zs)
		if err != nil {
			return nil, err
		}
		for _, z := range zs {
			out = append(out, Zone{ID: z.ID, Name: strings.ToLower(z.Name), Status: z.Status})
		}
		if env.ResultInfo == nil || page >= env.ResultInfo.TotalPages {
			break
		}
	}
	return out, nil
}

type cfRecord struct {
	ID      string `json:"id,omitempty"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	TTL     int    `json:"ttl"`
	Proxied *bool  `json:"proxied,omitempty"`
}

func (c *Cloudflare) toRecord(r cfRecord, zone Zone) Record {
	return Record{ID: r.ID, Type: strings.ToUpper(r.Type), Name: FQDN(r.Name, zone.Name), Content: r.Content, TTL: r.TTL,
		Proxied: r.Proxied != nil && *r.Proxied, Editable: isEditable(r.Type)}
}

func isEditable(t string) bool {
	t = strings.ToUpper(t)
	for _, e := range EditableTypes {
		if e == t {
			return true
		}
	}
	return false
}

func (c *Cloudflare) Records(ctx context.Context, zone Zone) ([]Record, error) {
	var out []Record
	for page := 1; page <= 200; page++ {
		var rs []cfRecord
		env, err := c.call(ctx, http.MethodGet, "/zones/"+url.PathEscape(zone.ID)+"/dns_records?per_page=500&page="+strconv.Itoa(page), nil, &rs)
		if err != nil {
			return nil, err
		}
		for _, r := range rs {
			out = append(out, c.toRecord(r, zone))
		}
		if env.ResultInfo == nil || page >= env.ResultInfo.TotalPages {
			break
		}
	}
	return out, nil
}

func cfBody(in RecordInput) (cfRecord, error) {
	if strings.Contains(in.Content, ",") {
		return cfRecord{}, fmt.Errorf("%w: Cloudflare records hold one address; create one record per address", ErrInvalid)
	}
	p := in.Proxied
	return cfRecord{Type: in.Type, Name: in.Name, Content: in.Content, TTL: in.TTL, Proxied: &p}, nil
}

func (c *Cloudflare) Create(ctx context.Context, zone Zone, in RecordInput) (*Record, error) {
	body, err := cfBody(in)
	if err != nil {
		return nil, err
	}
	var r cfRecord
	if _, err := c.call(ctx, http.MethodPost, "/zones/"+url.PathEscape(zone.ID)+"/dns_records", body, &r); err != nil {
		return nil, err
	}
	rec := c.toRecord(r, zone)
	return &rec, nil
}

func (c *Cloudflare) Update(ctx context.Context, zone Zone, cur *Record, in RecordInput) (*Record, error) {
	body, err := cfBody(in)
	if err != nil {
		return nil, err
	}
	var r cfRecord
	if _, err := c.call(ctx, http.MethodPut, "/zones/"+url.PathEscape(zone.ID)+"/dns_records/"+url.PathEscape(cur.ID), body, &r); err != nil {
		return nil, err
	}
	rec := c.toRecord(r, zone)
	return &rec, nil
}

func (c *Cloudflare) Delete(ctx context.Context, zone Zone, id string) error {
	_, err := c.call(ctx, http.MethodDelete, "/zones/"+url.PathEscape(zone.ID)+"/dns_records/"+url.PathEscape(id), nil, nil)
	return err
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
