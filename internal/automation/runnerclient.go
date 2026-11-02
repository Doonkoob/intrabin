package automation

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/intrabin/intrabin/internal/runner"
)

// Runner executes scripts on the isolated Action Runner.
type Runner interface {
	Execute(ctx context.Context, req *runner.Request) (*runner.Result, error)
	// Get returns the runner's record of an action; found is false when the runner never saw it.
	Get(ctx context.Context, id string) (res *runner.Result, found bool, err error)
	Health(ctx context.Context) (map[string]any, error)
}

// RunnerClient talks to an `intrabin runner` process over HTTP(S).
type RunnerClient struct {
	URL   string
	Token string
	HTTP  *http.Client
}

func (c *RunnerClient) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Transport: &http.Transport{Proxy: nil}}
}

func (c *RunnerClient) do(ctx context.Context, method, path string, in, out any) (int, error) {
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.URL, "/")+path, body)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client().Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode == http.StatusOK && out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			return resp.StatusCode, fmt.Errorf("runner returned an invalid response")
		}
	}
	return resp.StatusCode, nil
}

func (c *RunnerClient) Execute(ctx context.Context, req *runner.Request) (*runner.Result, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(req.TimeoutSeconds)*time.Second+30*time.Second)
	defer cancel()
	var res runner.Result
	code, err := c.do(ctx, http.MethodPost, "/v1/actions", req, &res)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, &rejectedError{code}
	}
	return &res, nil
}

// rejectedError means the runner refused the request before executing anything (bad request or
// authentication), so the action definitely did not run.
type rejectedError struct{ code int }

func (e *rejectedError) Error() string {
	return fmt.Sprintf("runner rejected the request (HTTP %d)", e.code)
}

func (c *RunnerClient) Get(ctx context.Context, id string) (*runner.Result, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var res runner.Result
	code, err := c.do(ctx, http.MethodGet, "/v1/actions/"+id, nil, &res)
	switch {
	case err != nil:
		return nil, false, err
	case code == http.StatusNotFound:
		return nil, false, nil
	case code != http.StatusOK:
		return nil, false, fmt.Errorf("runner returned HTTP %d", code)
	}
	return &res, true, nil
}

func (c *RunnerClient) Health(ctx context.Context) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out := map[string]any{}
	code, err := c.do(ctx, http.MethodGet, "/v1/health", nil, &out)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("runner returned HTTP %d", code)
	}
	return out, nil
}
