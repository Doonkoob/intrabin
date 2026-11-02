package dns

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/intrabin/intrabin/internal/dns/dnsmock"
)

func mockProviders(t *testing.T) (*dnsmock.Server, []Provider) {
	t.Helper()
	m := dnsmock.New("cf-token", "arvan-key")
	ts := httptest.NewServer(m)
	t.Cleanup(ts.Close)
	m.AddZone("cloudflare", "example.com")
	m.AddZone("arvan", "example.com")
	for _, p := range []string{"cloudflare", "arvan"} {
		m.AddRecord(p, "example.com", "A", "www", "192.0.2.10", 300, true)
		m.AddRecord(p, "example.com", "MX", "@", "mail.example.com", 300, false)
		for i := range 120 { // more than one page
			m.AddRecord(p, "example.com", "TXT", fmt.Sprintf("t%d", i), "v", 300, false)
		}
	}
	return m, []Provider{NewCloudflare(ts.URL+"/cloudflare/client/v4", "cf-token"), NewArvan(ts.URL+"/arvan/cdn/4.0", "Apikey arvan-key")}
}

func TestProvidersAgainstMock(t *testing.T) {
	ctx := context.Background()
	m, ps := mockProviders(t)
	for _, p := range ps {
		t.Run(p.Kind(), func(t *testing.T) {
			if err := p.Verify(ctx); err != nil {
				t.Fatal(err)
			}
			zs, err := p.Zones(ctx)
			if err != nil || len(zs) != 1 || zs[0].Name != "example.com" {
				t.Fatalf("zones %v %v", zs, err)
			}
			z := zs[0]
			rs, err := p.Records(ctx, z)
			if err != nil || len(rs) != 122 {
				t.Fatalf("records: %d %v", len(rs), err)
			}
			www := FindByName(rs, "A", "www.example.com")
			if len(www) != 1 || www[0].Content != "192.0.2.10" || !www[0].Proxied || !www[0].Editable {
				t.Fatalf("www %+v", www)
			}
			if mx := FindByName(rs, "MX", "example.com"); len(mx) != 1 || mx[0].Editable {
				t.Fatalf("MX must be listed read-only: %+v", mx)
			}
			in := RecordInput{Type: "a", Name: "www", Content: "198.51.100.7", TTL: 300, Proxied: true}
			if err := Validate(&in, z.Name); err != nil {
				t.Fatal(err)
			}
			up, err := p.Update(ctx, z, &www[0], in)
			if err != nil || up.Content != "198.51.100.7" || up.Name != "www.example.com" {
				t.Fatalf("update %+v %v", up, err)
			}
			cr := RecordInput{Type: "CNAME", Name: "api.example.com.", Content: "lb.example.net.", TTL: 0}
			if err := Validate(&cr, z.Name); err != nil {
				t.Fatal(err)
			}
			c, err := p.Create(ctx, z, cr)
			if err != nil || c.Content != "lb.example.net" || c.Name != "api.example.com" {
				t.Fatalf("create %+v %v", c, err)
			}
			rs, _ = p.Records(ctx, z)
			if !Matches(Find(rs, c.ID), cr) || !Matches(Find(rs, www[0].ID), in) {
				t.Fatal("read-back does not match")
			}
			if err := p.Delete(ctx, z, c.ID); err != nil {
				t.Fatal(err)
			}
			if err := p.Delete(ctx, z, c.ID); !errors.Is(err, ErrNotFound) {
				t.Fatalf("second delete: %v", err)
			}
		})
	}
	// ArvanCloud keeps provider-specific settings on update.
	for _, r := range m.Records("arvan", "example.com") {
		if r["name"] == "www" {
			if r["upstream_https"] != "default" || r["ip_filter_mode"] == nil {
				t.Fatalf("arvan settings lost on update: %v", r)
			}
			if r["type"] != "a" {
				t.Fatalf("arvan type %v", r["type"])
			}
		}
	}
}

func TestProviderErrors(t *testing.T) {
	ctx := context.Background()
	m := dnsmock.New("cf-token", "arvan-key")
	ts := httptest.NewServer(m)
	defer ts.Close()
	for _, p := range []Provider{NewCloudflare(ts.URL+"/cloudflare/client/v4", "wrong"), NewArvan(ts.URL+"/arvan/cdn/4.0", "wrong")} {
		if err := p.Verify(ctx); !errors.Is(err, ErrAuth) {
			t.Fatalf("%s: want ErrAuth, got %v", p.Kind(), err)
		}
	}
	m.AddZone("cloudflare", "example.com")
	cf := NewCloudflare(ts.URL+"/cloudflare/client/v4", "cf-token")
	zs, _ := cf.Zones(ctx)
	m.Fail("cloudflare", "POST", 400, false)
	in := RecordInput{Type: "A", Name: "x", Content: "192.0.2.1", TTL: 1}
	_ = Validate(&in, "example.com")
	_, err := cf.Create(ctx, zs[0], in)
	var pe *ProviderError
	if !errors.As(err, &pe) || pe.Status != 400 {
		t.Fatalf("want ProviderError 400, got %v", err)
	}
	if _, err := NewCloudflare("http://127.0.0.1:1", "x").Zones(ctx); !errors.As(err, new(*TransportError)) {
		t.Fatalf("want TransportError, got %v", err)
	}
}

func TestValidateAndNames(t *testing.T) {
	cases := []struct {
		in RecordInput
		ok bool
	}{
		{RecordInput{Type: "A", Name: "@", Content: "192.0.2.1"}, true},
		{RecordInput{Type: "A", Name: "x", Content: "2001:db8::1"}, false},
		{RecordInput{Type: "AAAA", Name: "x", Content: "2001:db8::1"}, true},
		{RecordInput{Type: "A", Name: "x", Content: "192.0.2.1, 192.0.2.2"}, true},
		{RecordInput{Type: "CNAME", Name: "x", Content: "not a host"}, false},
		{RecordInput{Type: "TXT", Name: "_acme-challenge", Content: "token"}, true},
		{RecordInput{Type: "MX", Name: "x", Content: "mail.example.com"}, false},
		{RecordInput{Type: "A", Name: "bad name", Content: "192.0.2.1"}, false},
		{RecordInput{Type: "A", Name: "x", Content: "192.0.2.1", TTL: 5}, false},
		{RecordInput{Type: "A", Name: "*.x", Content: "192.0.2.1"}, true},
	}
	for _, c := range cases {
		in := c.in
		if err := Validate(&in, "example.com"); (err == nil) != c.ok {
			t.Errorf("%+v: err=%v", c.in, err)
		}
	}
	if FQDN("@", "Example.com.") != "example.com" || FQDN("WWW", "example.com") != "www.example.com" || Relative("www.example.com", "example.com") != "www" || Relative("example.com", "example.com") != "@" {
		t.Fatal("name helpers")
	}
}
