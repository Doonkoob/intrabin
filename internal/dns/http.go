package dns

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"time"
)

type httpClient struct {
	base   string
	auth   string // Authorization header value
	client *http.Client
}

// RequestTimeout bounds each provider API request.
var RequestTimeout = 30 * time.Second

func newHTTPClient(base, auth string) *httpClient {
	return &httpClient{base: base, auth: auth, client: &http.Client{Timeout: RequestTimeout}}
}

// do performs one request. GET requests are retried on 429/5xx/transport errors with backoff;
// writes are never retried here (the caller decides, because a write may have been applied).
func (c *httpClient) do(ctx context.Context, method, path string, body any) (int, []byte, error) {
	var payload []byte
	if body != nil {
		payload, _ = json.Marshal(body)
	}
	attempts := 1
	if method == http.MethodGet {
		attempts = 3
	}
	var lastErr error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			delay := time.Duration(i) * time.Second
			select {
			case <-ctx.Done():
				return 0, nil, &TransportError{ctx.Err()}
			case <-time.After(delay):
			}
		}
		req, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(payload))
		if err != nil {
			return 0, nil, err
		}
		req.Header.Set("Authorization", c.auth)
		req.Header.Set("Accept", "application/json")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("User-Agent", "Intrabin")
		resp, err := c.client.Do(req)
		if err != nil {
			lastErr = &TransportError{err}
			continue
		}
		b, rerr := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		resp.Body.Close()
		if rerr != nil {
			lastErr = &TransportError{rerr}
			continue
		}
		if method == http.MethodGet && (resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500) && i+1 < attempts {
			if ra, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && ra > 0 && ra <= 10 {
				select {
				case <-ctx.Done():
				case <-time.After(time.Duration(ra) * time.Second):
				}
			}
			lastErr = nil
			continue
		}
		return resp.StatusCode, b, nil
	}
	if lastErr == nil {
		lastErr = ErrRateLimited
	}
	return 0, nil, lastErr
}
