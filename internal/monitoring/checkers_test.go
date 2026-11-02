package monitoring

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/intrabin/intrabin/internal/model"
)

func httpMon(target string) *model.Monitor {
	return &model.Monitor{ID: "m", Type: model.MonitorHTTP, Target: target, TimeoutSeconds: 2, Version: 1}
}

func TestHTTPSuccessAndStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/500":
			w.WriteHeader(503)
		case "/404":
			w.WriteHeader(404)
		default:
			w.Write([]byte("hello intrabin"))
		}
	}))
	defer srv.Close()
	p := NewProber("test")
	out, attempts := p.Probe(context.Background(), httpMon(srv.URL+"/"))
	if !out.Success || *out.HTTPStatus != 200 || attempts != 1 {
		t.Fatalf("%+v", out)
	}
	out, _ = p.Probe(context.Background(), httpMon(srv.URL+"/500"))
	if out.Success || out.Category != model.ErrHTTP5xx || *out.HTTPStatus != 503 {
		t.Fatalf("%+v", out)
	}
	out, _ = p.Probe(context.Background(), httpMon(srv.URL+"/404"))
	if out.Category != model.ErrUnexpectedStatus {
		t.Fatalf("%+v", out)
	}
	m := httpMon(srv.URL + "/404")
	m.Settings.ExpectedStatus = []string{"404"}
	if out, _ = p.Probe(context.Background(), m); !out.Success {
		t.Fatalf("expected status pattern ignored: %+v", out)
	}
	m = httpMon(srv.URL + "/")
	m.Settings.ExpectedBody = "nope"
	if out, _ = p.Probe(context.Background(), m); out.Category != model.ErrBodyMismatch {
		t.Fatalf("%+v", out)
	}
}

func TestHTTPTimeoutIsNotHTTP5xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(1500 * time.Millisecond)
	}))
	defer srv.Close()
	m := httpMon(srv.URL)
	m.TimeoutSeconds = 1
	out, _ := NewProber("t").Probe(context.Background(), m)
	if out.Success || out.Category != model.ErrTimeout || out.HTTPStatus != nil {
		t.Fatalf("timeout misclassified: %+v", out)
	}
}

func TestConnectionRefused(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := l.Addr().String()
	l.Close()
	out, _ := NewProber("t").Probe(context.Background(), httpMon("http://"+addr+"/"))
	if out.Category != model.ErrConnectionRefused {
		t.Fatalf("%+v", out)
	}
	tcp := &model.Monitor{Type: model.MonitorTCP, Target: addr, TimeoutSeconds: 1}
	out, _ = NewProber("t").Probe(context.Background(), tcp)
	if out.Category != model.ErrConnectionRefused {
		t.Fatalf("%+v", out)
	}
}

func TestRetries(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 2 {
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()
	m := httpMon(srv.URL)
	m.Retries = 2
	out, attempts := NewProber("t").Probe(context.Background(), m)
	if !out.Success || attempts != 2 {
		t.Fatalf("attempts=%d %+v", attempts, out)
	}
}

func TestOriginCheckWithHostHeader(t *testing.T) {
	var gotHost string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { gotHost = r.Host }))
	defer srv.Close()
	m := httpMon("http://www.example.test/health")
	m.Settings.OriginAddress = strings.TrimPrefix(srv.URL, "http://")
	out, _ := NewProber("t").Probe(context.Background(), m)
	if !out.Success || gotHost != "www.example.test" {
		t.Fatalf("origin check failed: %+v host=%q", out, gotHost)
	}
}

func TestTLSVerification(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	out, _ := NewProber("t").Probe(context.Background(), httpMon(srv.URL))
	if out.Category != model.ErrTLS {
		t.Fatalf("self-signed cert accepted by default: %+v", out)
	}
	m := httpMon(srv.URL)
	m.Settings.SkipTLSVerify = true
	out, _ = NewProber("t").Probe(context.Background(), m)
	if !out.Success || out.CertExpiresAt == nil {
		t.Fatalf("%+v", out)
	}
	tlsMon := &model.Monitor{Type: model.MonitorTLS, Target: strings.TrimPrefix(srv.URL, "https://"), TimeoutSeconds: 2, Settings: model.MonitorSettings{SkipTLSVerify: true}}
	p := NewProber("t")
	out, _ = p.Probe(context.Background(), tlsMon)
	if !out.Success || out.CertExpiresAt == nil {
		t.Fatalf("tls monitor: %+v", out)
	}
	// Pretend it is close to expiry.
	p.Now = func() time.Time { return out.CertExpiresAt.Add(-2 * 24 * time.Hour) }
	out, _ = p.Probe(context.Background(), tlsMon)
	if out.Success || out.Category != model.ErrCertExpiring {
		t.Fatalf("expiry not detected: %+v", out)
	}
}

func TestDNSFailure(t *testing.T) {
	m := &model.Monitor{Type: model.MonitorDNS, Target: "does-not-exist.invalid", TimeoutSeconds: 2}
	out, _ := NewProber("t").Probe(context.Background(), m)
	if out.Success || (out.Category != model.ErrDNS && out.Category != model.ErrTimeout) {
		t.Fatalf("%+v", out)
	}
	m = &model.Monitor{Type: model.MonitorDNS, Target: "localhost", TimeoutSeconds: 2, Settings: model.MonitorSettings{RecordType: "A", ExpectedValue: "127.0.0.1"}}
	out, _ = NewProber("t").Probe(context.Background(), m)
	if !out.Success {
		t.Logf("localhost lookup not available in this environment: %+v", out)
	}
}

func TestStatusMatches(t *testing.T) {
	cases := []struct {
		code int
		pats []string
		want bool
	}{
		{200, nil, true}, {301, nil, true}, {404, nil, false}, {500, nil, false},
		{204, []string{"2xx"}, true}, {404, []string{"200-299", "404"}, true}, {418, []string{"400-417"}, false},
	}
	for _, c := range cases {
		if got := StatusMatches(c.code, c.pats); got != c.want {
			t.Errorf("StatusMatches(%d,%v)=%v", c.code, c.pats, got)
		}
	}
	if ValidStatusPattern("6xx") || ValidStatusPattern("abc") || !ValidStatusPattern("2xx") || !ValidStatusPattern("200-204") {
		t.Fatal("pattern validation")
	}
}
