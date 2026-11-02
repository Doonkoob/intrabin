// Package runner implements the isolated Action Runner: a separate process (normally a separate
// container) that executes workflow scripts (Bash or Python) for the leader. The API/control-plane
// process never executes user scripts itself.
//
// Isolation measures: separate process and container (no Docker socket, read-only root, dropped
// capabilities, non-root user), bearer-token authentication, per-action working directory, minimal
// environment, resource limits applied by re-executing this binary as a small wrapper
// (`intrabin runner-exec`) that sets rlimits and then execs the interpreter, process-group kill on
// timeout, output size cap, bounded concurrency. Results are kept by action ID so that a repeated
// request for the same action returns the recorded result instead of running the script again.
package runner

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Request is one script execution.
type Request struct {
	ID             string            `json:"id"` // action ID (idempotency key)
	Language       string            `json:"language"`
	Script         string            `json:"script"`
	Env            map[string]string `json:"env"`
	TimeoutSeconds int               `json:"timeout_seconds"`
}

// Result is the outcome of an execution.
type Result struct {
	ID         string `json:"id"`
	Status     string `json:"status"` // running | succeeded | failed | timeout | interrupted
	ExitCode   *int   `json:"exit_code,omitempty"`
	Output     string `json:"output"`
	Truncated  bool   `json:"truncated"`
	Error      string `json:"error,omitempty"`
	DurationMs int64  `json:"duration_ms"`
	FinishedAt string `json:"finished_at,omitempty"`
}

// Config configures a runner server.
type Config struct {
	Token         string
	WorkDir       string
	MaxConcurrent int
	MaxOutput     int
	MaxTimeout    time.Duration
	// Limits applied to every script.
	CPUSeconds  uint64
	MemoryBytes uint64
	MaxProcs    uint64
	MaxFileSize uint64
	// SelfExe is the path of the intrabin binary used as the rlimit wrapper (default: os.Executable).
	SelfExe string
	Log     *slog.Logger
}

// Server executes scripts.
type Server struct {
	cfg     Config
	sem     chan struct{}
	mu      sync.Mutex
	running map[string]chan struct{}
}

var (
	idRe  = regexp.MustCompile(`^[a-zA-Z0-9-]{8,64}$`)
	envRe = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,63}$`)
)

// New validates the configuration and creates a server.
func New(cfg Config) (*Server, error) {
	if len(cfg.Token) < 24 {
		return nil, errors.New("runner token must be at least 24 characters")
	}
	if cfg.WorkDir == "" {
		cfg.WorkDir = filepath.Join(os.TempDir(), "intrabin-runner")
	}
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = 4
	}
	if cfg.MaxOutput <= 0 {
		cfg.MaxOutput = 64 << 10
	}
	if cfg.MaxTimeout <= 0 {
		cfg.MaxTimeout = 10 * time.Minute
	}
	if cfg.CPUSeconds == 0 {
		cfg.CPUSeconds = 60
	}
	if cfg.MemoryBytes == 0 {
		cfg.MemoryBytes = 512 << 20
	}
	if cfg.MaxProcs == 0 {
		cfg.MaxProcs = 64
	}
	if cfg.MaxFileSize == 0 {
		cfg.MaxFileSize = 64 << 20
	}
	if cfg.SelfExe == "" {
		exe, err := os.Executable()
		if err != nil {
			return nil, err
		}
		cfg.SelfExe = exe
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	for _, d := range []string{cfg.WorkDir, filepath.Join(cfg.WorkDir, "results"), filepath.Join(cfg.WorkDir, "jobs")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}
	return &Server{cfg: cfg, sem: make(chan struct{}, cfg.MaxConcurrent), running: map[string]chan struct{}{}}, nil
}

// Handler returns the HTTP API.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"status": "ok", "interpreters": interpreters()})
	})
	mux.HandleFunc("POST /v1/actions", s.auth(s.handleExecute))
	mux.HandleFunc("GET /v1/actions/{id}", s.auth(s.handleGet))
	return mux
}

func (s *Server) auth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(tok), []byte(s.cfg.Token)) != 1 {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		h(w, r)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) resultPath(id string) string {
	return filepath.Join(s.cfg.WorkDir, "results", id+".json")
}

func (s *Server) startedPath(id string) string {
	return filepath.Join(s.cfg.WorkDir, "results", id+".started")
}

// load returns the recorded result of an action. An action that was started by an earlier runner
// process but never recorded a result (the runner crashed or was restarted mid-execution) is
// reported as "interrupted": it may have had partial effects and is never run again.
func (s *Server) load(id string) (*Result, bool) {
	b, err := os.ReadFile(s.resultPath(id))
	if err != nil {
		if _, serr := os.Stat(s.startedPath(id)); serr == nil {
			s.mu.Lock()
			_, running := s.running[id]
			s.mu.Unlock()
			if !running {
				return &Result{ID: id, Status: "interrupted", Error: "the runner stopped while this action was executing; its outcome is unknown"}, true
			}
		}
		return nil, false
	}
	var r Result
	if json.Unmarshal(b, &r) != nil {
		return nil, false
	}
	return &r, true
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !idRe.MatchString(id) {
		writeJSON(w, 400, map[string]string{"error": "invalid id"})
		return
	}
	if res, ok := s.load(id); ok {
		writeJSON(w, 200, res)
		return
	}
	s.mu.Lock()
	_, running := s.running[id]
	s.mu.Unlock()
	if running {
		writeJSON(w, 200, Result{ID: id, Status: "running"})
		return
	}
	writeJSON(w, 404, map[string]string{"error": "unknown action"})
}

func (s *Server) handleExecute(w http.ResponseWriter, r *http.Request) {
	var req Request
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid request"})
		return
	}
	if err := s.validate(&req); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	res := s.Execute(r.Context(), &req)
	writeJSON(w, 200, res)
}

func (s *Server) validate(req *Request) error {
	if !idRe.MatchString(req.ID) {
		return errors.New("invalid action id")
	}
	if req.Language != "bash" && req.Language != "python" {
		return errors.New("language must be bash or python")
	}
	if strings.TrimSpace(req.Script) == "" || len(req.Script) > 256<<10 {
		return errors.New("script is empty or too large")
	}
	for k, v := range req.Env {
		if !envRe.MatchString(k) || strings.HasPrefix(k, "LD_") || k == "PATH" || k == "HOME" || len(v) > 8192 {
			return fmt.Errorf("environment variable %q is not allowed", k)
		}
	}
	if req.TimeoutSeconds <= 0 {
		req.TimeoutSeconds = int(min(60*time.Second, s.cfg.MaxTimeout) / time.Second)
	}
	if time.Duration(req.TimeoutSeconds)*time.Second > s.cfg.MaxTimeout {
		return fmt.Errorf("timeout exceeds the runner maximum of %s", s.cfg.MaxTimeout)
	}
	return nil
}

// Execute runs the request once per action ID. Concurrent or repeated requests for the same ID
// wait for and return the first execution's result.
func (s *Server) Execute(ctx context.Context, req *Request) *Result {
	for {
		if res, ok := s.load(req.ID); ok {
			return res
		}
		s.mu.Lock()
		ch, busy := s.running[req.ID]
		if !busy {
			ch = make(chan struct{})
			s.running[req.ID] = ch
			s.mu.Unlock()
			break
		}
		s.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return &Result{ID: req.ID, Status: "running"}
		}
	}
	defer func() {
		s.mu.Lock()
		close(s.running[req.ID])
		delete(s.running, req.ID)
		s.mu.Unlock()
	}()
	s.sem <- struct{}{}
	defer func() { <-s.sem }()
	if res, ok := s.load(req.ID); ok { // recorded by a racing request just before we registered
		return res
	}
	if _, err := os.Stat(s.startedPath(req.ID)); err == nil {
		// Started by an earlier runner process that never recorded a result: never run it again.
		return &Result{ID: req.ID, Status: "interrupted", Error: "the runner stopped while this action was executing; its outcome is unknown"}
	}
	// Durable "started" marker: written before the script can have any effect.
	if err := os.WriteFile(s.startedPath(req.ID), nil, 0o600); err != nil {
		return &Result{ID: req.ID, Status: "failed", Error: "cannot record action start"}
	}
	res := s.run(req)
	b, _ := json.Marshal(res)
	tmp := s.resultPath(req.ID) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err == nil {
		_ = os.Rename(tmp, s.resultPath(req.ID))
	}
	code := -1
	if res.ExitCode != nil {
		code = *res.ExitCode
	}
	s.cfg.Log.Info("action finished", "action_id", req.ID, "language", req.Language, "status", res.Status, "exit_code", code, "duration_ms", res.DurationMs)
	return res
}

func interpreters() map[string]string {
	out := map[string]string{}
	for lang, names := range map[string][]string{"bash": {"bash"}, "python": {"python3", "python"}} {
		for _, n := range names {
			if p, err := exec.LookPath(n); err == nil {
				out[lang] = p
				break
			}
		}
	}
	return out
}

type capWriter struct {
	buf       bytes.Buffer
	max       int
	truncated bool
	mu        sync.Mutex
}

func (c *capWriter) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if room := c.max - c.buf.Len(); room > 0 {
		if len(p) > room {
			c.buf.Write(p[:room])
			c.truncated = true
		} else {
			c.buf.Write(p)
		}
	} else if len(p) > 0 {
		c.truncated = true
	}
	return len(p), nil
}

func (s *Server) run(req *Request) *Result {
	start := time.Now()
	res := &Result{ID: req.ID}
	finish := func() *Result {
		res.DurationMs = time.Since(start).Milliseconds()
		res.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
		return res
	}
	interp := interpreters()[req.Language]
	if interp == "" {
		res.Status, res.Error = "failed", req.Language+" is not installed in the runner"
		return finish()
	}
	dir, err := os.MkdirTemp(filepath.Join(s.cfg.WorkDir, "jobs"), "job-")
	if err != nil {
		res.Status, res.Error = "failed", "cannot create work directory"
		return finish()
	}
	defer os.RemoveAll(dir)
	ext := map[string]string{"bash": ".sh", "python": ".py"}[req.Language]
	script := filepath.Join(dir, "script"+ext)
	if err := os.WriteFile(script, []byte(req.Script), 0o600); err != nil {
		res.Status, res.Error = "failed", "cannot write script"
		return finish()
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(req.TimeoutSeconds)*time.Second)
	defer cancel()
	args := []string{"runner-exec",
		"-cpu", fmt.Sprint(s.cfg.CPUSeconds), "-mem", fmt.Sprint(s.cfg.MemoryBytes), "-nproc", fmt.Sprint(s.cfg.MaxProcs), "-fsize", fmt.Sprint(s.cfg.MaxFileSize),
		"--", interp, script}
	cmd := exec.CommandContext(ctx, s.cfg.SelfExe, args...)
	cmd.Dir = dir
	env := []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "HOME=" + dir, "LANG=C.UTF-8", "TMPDIR=" + dir}
	for k, v := range req.Env {
		env = append(env, k+"="+v)
	}
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second
	out := &capWriter{max: s.cfg.MaxOutput}
	cmd.Stdout, cmd.Stderr = out, out
	cmd.Stdin = nil
	err = cmd.Run()
	res.Output, res.Truncated = out.buf.String(), out.truncated
	if ctx.Err() == context.DeadlineExceeded {
		res.Status, res.Error = "timeout", fmt.Sprintf("killed after %ds timeout", req.TimeoutSeconds)
		return finish()
	}
	if cmd.ProcessState != nil {
		code := cmd.ProcessState.ExitCode()
		res.ExitCode = &code
	}
	if err != nil {
		res.Status = "failed"
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			res.Error = err.Error()
		}
		return finish()
	}
	res.Status = "succeeded"
	return finish()
}
