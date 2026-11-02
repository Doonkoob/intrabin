// Package monitoring implements probes, round evaluation and the probe scheduler.
package monitoring

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/intrabin/intrabin/internal/model"
	"github.com/intrabin/intrabin/internal/security"
)

// Outcome is the result of a single probe attempt.
type Outcome struct {
	Success       bool
	Slow          bool
	Latency       time.Duration
	HTTPStatus    *int
	Category      model.ErrorCategory
	Message       string
	CertExpiresAt *time.Time
}

// Prober executes checks. Dialer/Resolver are injectable for tests.
type Prober struct {
	UserAgent string
	// Now is used for certificate expiry evaluation.
	Now func() time.Time
}

func NewProber(version string) *Prober {
	return &Prober{UserAgent: "Intrabin/" + version + " (+https://github.com/intrabin/intrabin)", Now: time.Now}
}

// Probe runs one check with in-probe retries and returns the final outcome and attempts.
func (p *Prober) Probe(ctx context.Context, m *model.Monitor) (Outcome, int) {
	attempts := 0
	var out Outcome
	for {
		attempts++
		out = p.once(ctx, m)
		if out.Success || attempts > m.Retries || ctx.Err() != nil {
			return out, attempts
		}
		select {
		case <-ctx.Done():
			return out, attempts
		case <-time.After(time.Second):
		}
	}
}

func (p *Prober) once(parent context.Context, m *model.Monitor) Outcome {
	timeout := time.Duration(m.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	var out Outcome
	switch m.Type {
	case model.MonitorHTTP:
		out = p.checkHTTP(ctx, m)
	case model.MonitorTCP:
		out = p.checkTCP(ctx, m)
	case model.MonitorDNS:
		out = p.checkDNS(ctx, m)
	case model.MonitorTLS:
		out = p.checkTLS(ctx, m)
	default:
		out = Outcome{Category: model.ErrInvalidConfig, Message: "unsupported monitor type"}
	}
	out.Message = security.RedactString(out.Message)
	if out.Success && m.Settings.DegradedLatencyMs > 0 && out.Latency > time.Duration(m.Settings.DegradedLatencyMs)*time.Millisecond {
		out.Slow = true
	}
	return out
}

func (p *Prober) dialer() *net.Dialer {
	return &net.Dialer{KeepAlive: -1}
}

// originDial returns a DialContext that connects to origin instead of the URL host.
func originDial(d *net.Dialer, origin string) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		if origin == "" {
			return d.DialContext(ctx, network, addr)
		}
		_, port, _ := net.SplitHostPort(addr)
		target := origin
		if _, _, err := net.SplitHostPort(origin); err != nil {
			target = net.JoinHostPort(origin, port)
		}
		return d.DialContext(ctx, network, target)
	}
}

func (p *Prober) checkHTTP(ctx context.Context, m *model.Monitor) Outcome {
	s := m.Settings
	u, err := url.Parse(m.Target)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return Outcome{Category: model.ErrInvalidConfig, Message: "target must be an http(s) URL"}
	}
	tlsCfg := &tls.Config{InsecureSkipVerify: s.SkipTLSVerify, MinVersion: tls.VersionTLS12} // #nosec G402 -- opt-in per monitor
	if s.TLSServerName != "" {
		tlsCfg.ServerName = s.TLSServerName
	}
	tr := &http.Transport{
		DialContext:           originDial(p.dialer(), s.OriginAddress),
		TLSClientConfig:       tlsCfg,
		DisableKeepAlives:     true,
		ForceAttemptHTTP2:     true,
		ResponseHeaderTimeout: time.Duration(m.TimeoutSeconds) * time.Second,
		Proxy:                 nil, // probes must reflect direct reachability
	}
	defer tr.CloseIdleConnections()
	follow := s.FollowRedirects == nil || *s.FollowRedirects
	maxRedirects := s.MaxRedirects
	if maxRedirects <= 0 {
		maxRedirects = 5
	}
	client := &http.Client{Transport: tr, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if !follow {
			return http.ErrUseLastResponse
		}
		if len(via) >= maxRedirects {
			return fmt.Errorf("stopped after %d redirects", maxRedirects)
		}
		return nil
	}}
	method := strings.ToUpper(s.Method)
	if method == "" {
		method = http.MethodGet
	}
	var body io.Reader
	if s.Body != "" {
		body = strings.NewReader(s.Body)
	}
	req, err := http.NewRequestWithContext(ctx, method, m.Target, body)
	if err != nil {
		return Outcome{Category: model.ErrInvalidConfig, Message: err.Error()}
	}
	req.Header.Set("User-Agent", p.UserAgent)
	for k, v := range s.Headers {
		req.Header.Set(k, v)
	}
	if m.Secrets != nil {
		for k, v := range m.Secrets.SecretHeaders {
			req.Header.Set(k, v)
		}
		if m.Secrets.BasicAuthUser != "" {
			req.SetBasicAuth(m.Secrets.BasicAuthUser, m.Secrets.BasicAuthPassword)
		}
	}
	if s.HostHeader != "" {
		req.Host = s.HostHeader
	}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return Outcome{Latency: time.Since(start), Category: Classify(err), Message: err.Error()}
	}
	defer resp.Body.Close()
	var bodyBytes []byte
	if s.ExpectedBody != "" {
		bodyBytes, err = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if err != nil {
			code := resp.StatusCode
			return Outcome{Latency: time.Since(start), HTTPStatus: &code, Category: Classify(err), Message: "reading body: " + err.Error()}
		}
	} else {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	}
	lat := time.Since(start)
	code := resp.StatusCode
	out := Outcome{Latency: lat, HTTPStatus: &code}
	if resp.TLS != nil && len(resp.TLS.PeerCertificates) > 0 {
		exp := resp.TLS.PeerCertificates[0].NotAfter.UTC()
		out.CertExpiresAt = &exp
	}
	if !StatusMatches(code, s.ExpectedStatus) {
		if code >= 500 && code <= 599 {
			out.Category = model.ErrHTTP5xx
		} else {
			out.Category = model.ErrUnexpectedStatus
		}
		out.Message = fmt.Sprintf("HTTP %d %s", code, http.StatusText(code))
		return out
	}
	if s.ExpectedBody != "" && !strings.Contains(string(bodyBytes), s.ExpectedBody) {
		out.Category = model.ErrBodyMismatch
		out.Message = "response body does not contain the expected text"
		return out
	}
	if cat, msg := p.certCheck(out.CertExpiresAt, s); cat != "" {
		out.Category, out.Message = cat, msg
		return out
	}
	out.Success = true
	return out
}

func (p *Prober) certCheck(exp *time.Time, s model.MonitorSettings) (model.ErrorCategory, string) {
	if exp == nil || s.CertExpiryFailDays <= 0 {
		return "", ""
	}
	left := exp.Sub(p.Now())
	if left < time.Duration(s.CertExpiryFailDays)*24*time.Hour {
		return model.ErrCertExpiring, fmt.Sprintf("certificate expires in %.1f days (threshold %d)", left.Hours()/24, s.CertExpiryFailDays)
	}
	return "", ""
}

// StatusMatches checks a status code against patterns like "200", "2xx", "200-299".
// An empty pattern list accepts 200-399.
func StatusMatches(code int, patterns []string) bool {
	if len(patterns) == 0 {
		return code >= 200 && code < 400
	}
	for _, p := range patterns {
		p = strings.TrimSpace(strings.ToLower(p))
		if len(p) == 3 && strings.HasSuffix(p, "xx") && p[0] >= '1' && p[0] <= '5' {
			if code/100 == int(p[0]-'0') {
				return true
			}
			continue
		}
		if lo, hi, ok := strings.Cut(p, "-"); ok {
			l, e1 := strconv.Atoi(strings.TrimSpace(lo))
			h, e2 := strconv.Atoi(strings.TrimSpace(hi))
			if e1 == nil && e2 == nil && code >= l && code <= h {
				return true
			}
			continue
		}
		if n, err := strconv.Atoi(p); err == nil && n == code {
			return true
		}
	}
	return false
}

// ValidStatusPattern reports whether a pattern is syntactically valid.
func ValidStatusPattern(p string) bool {
	p = strings.TrimSpace(strings.ToLower(p))
	if len(p) == 3 && strings.HasSuffix(p, "xx") && p[0] >= '1' && p[0] <= '5' {
		return true
	}
	if lo, hi, ok := strings.Cut(p, "-"); ok {
		l, e1 := strconv.Atoi(strings.TrimSpace(lo))
		h, e2 := strconv.Atoi(strings.TrimSpace(hi))
		return e1 == nil && e2 == nil && l >= 100 && h <= 599 && l <= h
	}
	n, err := strconv.Atoi(p)
	return err == nil && n >= 100 && n <= 599
}

func (p *Prober) checkTCP(ctx context.Context, m *model.Monitor) Outcome {
	start := time.Now()
	conn, err := p.dialer().DialContext(ctx, "tcp", m.Target)
	lat := time.Since(start)
	if err != nil {
		return Outcome{Latency: lat, Category: Classify(err), Message: err.Error()}
	}
	conn.Close()
	return Outcome{Success: true, Latency: lat}
}

func (p *Prober) checkTLS(ctx context.Context, m *model.Monitor) Outcome {
	s := m.Settings
	addr := m.Target
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
		addr = net.JoinHostPort(addr, "443")
	}
	sni := host
	if s.TLSServerName != "" {
		sni = s.TLSServerName
	}
	dialAddr := addr
	if s.OriginAddress != "" {
		dialAddr = s.OriginAddress
		if _, _, err := net.SplitHostPort(dialAddr); err != nil {
			_, port, _ := net.SplitHostPort(addr)
			dialAddr = net.JoinHostPort(dialAddr, port)
		}
	}
	start := time.Now()
	d := tls.Dialer{NetDialer: p.dialer(), Config: &tls.Config{ServerName: sni, InsecureSkipVerify: s.SkipTLSVerify, MinVersion: tls.VersionTLS12}} // #nosec G402 -- opt-in
	conn, err := d.DialContext(ctx, "tcp", dialAddr)
	lat := time.Since(start)
	if err != nil {
		return Outcome{Latency: lat, Category: Classify(err), Message: err.Error()}
	}
	defer conn.Close()
	state := conn.(*tls.Conn).ConnectionState()
	if len(state.PeerCertificates) == 0 {
		return Outcome{Latency: lat, Category: model.ErrTLS, Message: "no peer certificate"}
	}
	exp := state.PeerCertificates[0].NotAfter.UTC()
	out := Outcome{Latency: lat, CertExpiresAt: &exp}
	failDays := s.CertExpiryFailDays
	if failDays <= 0 {
		failDays = 7
	}
	s.CertExpiryFailDays = failDays
	if cat, msg := p.certCheck(&exp, s); cat != "" {
		out.Category, out.Message = cat, msg
		return out
	}
	warn := s.CertExpiryWarnDays
	if warn <= 0 {
		warn = 21
	}
	if exp.Sub(p.Now()) < time.Duration(warn)*24*time.Hour {
		out.Slow = true // surfaced as DEGRADED
		out.Message = fmt.Sprintf("certificate expires in %.1f days", exp.Sub(p.Now()).Hours()/24)
	}
	out.Success = true
	return out
}

func (p *Prober) checkDNS(ctx context.Context, m *model.Monitor) Outcome {
	s := m.Settings
	r := net.DefaultResolver
	if s.Resolver != "" {
		server := s.Resolver
		if _, _, err := net.SplitHostPort(server); err != nil {
			server = net.JoinHostPort(server, "53")
		}
		r = &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return p.dialer().DialContext(ctx, network, server)
		}}
	}
	host := m.Target
	rt := strings.ToUpper(s.RecordType)
	if rt == "" {
		rt = "A"
	}
	start := time.Now()
	var values []string
	var err error
	switch rt {
	case "A", "AAAA":
		var ips []net.IP
		network := "ip4"
		if rt == "AAAA" {
			network = "ip6"
		}
		ips, err = r.LookupIP(ctx, network, host)
		for _, ip := range ips {
			values = append(values, ip.String())
		}
	case "CNAME":
		var c string
		c, err = r.LookupCNAME(ctx, host)
		values = []string{strings.TrimSuffix(c, ".")}
	case "MX":
		var mx []*net.MX
		mx, err = r.LookupMX(ctx, host)
		for _, x := range mx {
			values = append(values, strings.TrimSuffix(x.Host, "."))
		}
	case "TXT":
		values, err = r.LookupTXT(ctx, host)
	case "NS":
		var ns []*net.NS
		ns, err = r.LookupNS(ctx, host)
		for _, x := range ns {
			values = append(values, strings.TrimSuffix(x.Host, "."))
		}
	default:
		return Outcome{Category: model.ErrInvalidConfig, Message: "unsupported record type " + rt}
	}
	lat := time.Since(start)
	if err != nil {
		return Outcome{Latency: lat, Category: Classify(err), Message: err.Error()}
	}
	if len(values) == 0 {
		return Outcome{Latency: lat, Category: model.ErrDNS, Message: "no records returned"}
	}
	if s.ExpectedValue != "" {
		found := false
		for _, v := range values {
			if strings.EqualFold(strings.TrimSuffix(v, "."), strings.TrimSuffix(s.ExpectedValue, ".")) {
				found = true
				break
			}
		}
		if !found {
			return Outcome{Latency: lat, Category: model.ErrDNSMismatch, Message: fmt.Sprintf("expected %q, got %s", s.ExpectedValue, strings.Join(values, ", "))}
		}
	}
	return Outcome{Success: true, Latency: lat, Message: strings.Join(values, ", ")}
}
