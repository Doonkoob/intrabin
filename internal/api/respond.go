package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/intrabin/intrabin/internal/storage"
)

// APIError is the uniform error body: {"error": {"code": "...", "message": "...", "fields": {...}}}.
type APIError struct {
	Status  int               `json:"-"`
	Code    string            `json:"code"`
	Message string            `json:"message"`
	Fields  map[string]string `json:"fields,omitempty"`
}

func (e *APIError) Error() string { return e.Message }

func errBadRequest(msg string) *APIError {
	return &APIError{Status: http.StatusBadRequest, Code: "bad_request", Message: msg}
}

func errValidation(fields map[string]string) *APIError {
	return &APIError{Status: http.StatusUnprocessableEntity, Code: "validation_failed", Message: "Some fields are invalid.", Fields: fields}
}

var (
	errUnauthorized = &APIError{Status: http.StatusUnauthorized, Code: "unauthorized", Message: "Authentication required."}
	errForbidden    = &APIError{Status: http.StatusForbidden, Code: "forbidden", Message: "You do not have permission to perform this action."}
	errNotFound     = &APIError{Status: http.StatusNotFound, Code: "not_found", Message: "Resource not found."}
	errConflict     = &APIError{Status: http.StatusConflict, Code: "conflict", Message: "Resource already exists."}
	errCSRF         = &APIError{Status: http.StatusForbidden, Code: "csrf_failed", Message: "Missing or invalid CSRF token."}
	errReadOnlyNode = &APIError{Status: http.StatusConflict, Code: "node_read_only", Message: "This node is not the active controller; global configuration is read-only here."}
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

func (s *Server) writeErr(w http.ResponseWriter, r *http.Request, err error) {
	var ae *APIError
	switch {
	case errors.As(err, &ae):
	case errors.Is(err, storage.ErrNotFound):
		ae = errNotFound
	case errors.Is(err, storage.ErrConflict):
		ae = errConflict
	default:
		// Never leak internal error details to clients.
		s.log.Error("internal error", "path", r.URL.Path, "method", r.Method, "request_id", requestID(r.Context()), "err", err)
		ae = &APIError{Status: http.StatusInternalServerError, Code: "internal", Message: "Internal server error. Request ID: " + requestID(r.Context())}
	}
	writeJSON(w, ae.Status, map[string]any{"error": ae})
}

// decode reads a JSON body (max 1 MiB) and rejects unknown fields.
func decode(r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(nil, r.Body, 1<<20)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		if errors.Is(err, io.EOF) {
			return errBadRequest("Request body is required.")
		}
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return errBadRequest("Request body too large.")
		}
		return errBadRequest(fmt.Sprintf("Invalid JSON: %s", strings.TrimPrefix(err.Error(), "json: ")))
	}
	return nil
}

func queryInt(r *http.Request, k string, def int) int {
	if v := r.URL.Query().Get(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func queryTime(r *http.Request, k string) (*time.Time, error) {
	v := r.URL.Query().Get(k)
	if v == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return nil, errBadRequest(k + " must be an RFC 3339 timestamp")
	}
	return &t, nil
}

// parseRange accepts 1h, 24h, 7d, 30d, 90d.
func parseRange(v string, def time.Duration) time.Duration {
	switch v {
	case "1h":
		return time.Hour
	case "6h":
		return 6 * time.Hour
	case "24h", "1d":
		return 24 * time.Hour
	case "7d":
		return 7 * 24 * time.Hour
	case "30d":
		return 30 * 24 * time.Hour
	case "90d":
		return 90 * 24 * time.Hour
	}
	return def
}
