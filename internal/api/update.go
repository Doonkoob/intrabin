package api

import (
	"errors"
	"net/http"

	"github.com/intrabin/intrabin/internal/update"
)

// Updates are node-local: every node checks GitHub and is updated on its own server.

func (s *Server) updateOrErr(w http.ResponseWriter, r *http.Request) bool {
	if s.update == nil {
		s.writeErr(w, r, &APIError{Status: http.StatusNotImplemented, Code: "updates_unavailable", Message: "Update checks are not available on this node."})
		return false
	}
	return true
}

func (s *Server) handleUpdateState(w http.ResponseWriter, r *http.Request) {
	if !s.updateOrErr(w, r) {
		return
	}
	writeJSON(w, 200, map[string]any{"update": s.update.State(r.Context())})
}

func (s *Server) handleUpdateCheck(w http.ResponseWriter, r *http.Request) {
	if !s.updateOrErr(w, r) {
		return
	}
	if err := s.update.Check(r.Context()); errors.Is(err, update.ErrDisabled) {
		s.writeErr(w, r, &APIError{Status: http.StatusConflict, Code: "updates_disabled", Message: err.Error()})
		return
	}
	// A failed check is reported in the state (check_error), not as a request error.
	writeJSON(w, 200, map[string]any{"update": s.update.State(r.Context())})
}

func (s *Server) handleUpdateDismiss(w http.ResponseWriter, r *http.Request) {
	if !s.updateOrErr(w, r) {
		return
	}
	if _, err := s.update.Dismiss(r.Context()); err != nil {
		s.writeErr(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"update": s.update.State(r.Context())})
}

type updateApplyReq struct {
	Version string `json:"version"`
}

func (s *Server) handleUpdateApply(w http.ResponseWriter, r *http.Request) {
	if !s.updateOrErr(w, r) {
		return
	}
	var req updateApplyReq
	if err := decode(r, &req); err != nil {
		s.writeErr(w, r, err)
		return
	}
	p := principal(r)
	err := s.update.Request(r.Context(), req.Version, p.User.Username)
	details := map[string]any{"version": req.Version, "from": s.update.Current}
	if err != nil {
		details["error"] = err.Error()
		s.audit.Record(r.Context(), p.actor(), "system.update_requested", "system", "", "failure", details)
		switch {
		case errors.Is(err, update.ErrNoAgent):
			s.writeErr(w, r, &APIError{Status: http.StatusConflict, Code: "update_agent_missing", Message: err.Error()})
		case errors.Is(err, update.ErrNotAvailable):
			s.writeErr(w, r, &APIError{Status: http.StatusConflict, Code: "update_not_available", Message: err.Error()})
		case errors.Is(err, update.ErrBusy):
			s.writeErr(w, r, &APIError{Status: http.StatusConflict, Code: "update_in_progress", Message: err.Error()})
		default:
			s.writeErr(w, r, err)
		}
		return
	}
	s.audit.Record(r.Context(), p.actor(), "system.update_requested", "system", "", "", details)
	writeJSON(w, 202, map[string]any{"update": s.update.State(r.Context())})
}
