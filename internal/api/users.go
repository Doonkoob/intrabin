package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/intrabin/intrabin/internal/auth"
	"github.com/intrabin/intrabin/internal/model"
	"github.com/intrabin/intrabin/internal/security"
)

func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	us, err := s.store.ListUsers(r.Context())
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	if us == nil {
		us = []*model.User{}
	}
	writeJSON(w, 200, map[string]any{"users": us})
}

type createUserReq struct {
	Username    string     `json:"username"`
	DisplayName string     `json:"display_name"`
	Password    string     `json:"password"`
	Role        model.Role `json:"role"`
	Language    string     `json:"language"`
}

func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	var req createUserReq
	if err := decode(r, &req); err != nil {
		s.writeErr(w, r, err)
		return
	}
	fields := map[string]string{}
	if !usernameRe.MatchString(req.Username) {
		fields["username"] = "3–64 characters: letters, digits, dot, dash, underscore."
	}
	if err := security.ValidatePassword(req.Password); err != nil {
		fields["password"] = err.Error()
	}
	if !req.Role.Valid() {
		fields["role"] = "Must be owner, admin, operator or viewer."
	}
	if req.Language == "" {
		req.Language = "en"
	}
	if !validLanguage(req.Language) {
		fields["language"] = "Must be en or fa."
	}
	if len(req.DisplayName) > 80 {
		fields["display_name"] = "At most 80 characters."
	}
	if len(fields) > 0 {
		s.writeErr(w, r, errValidation(fields))
		return
	}
	if !auth.CanManageUser(p.User.Role, req.Role, req.Role) {
		s.writeErr(w, r, errForbidden)
		return
	}
	hash, err := security.HashPassword(req.Password, s.argon)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	now := time.Now().UTC()
	u := &model.User{ID: uuid.NewString(), Username: req.Username, DisplayName: strings.TrimSpace(req.DisplayName), PasswordHash: hash, Role: req.Role,
		Language: req.Language, CreatedAt: now, UpdatedAt: now, PasswordChangedAt: now}
	if err := s.store.CreateUser(r.Context(), u); err != nil {
		s.writeErr(w, r, err)
		return
	}
	s.audit.Record(r.Context(), p.actor(), "user.create", "user", u.ID, "", map[string]any{"username": u.Username, "role": u.Role})
	writeJSON(w, 201, map[string]any{"user": u})
}

type updateUserReq struct {
	DisplayName *string     `json:"display_name"`
	Role        *model.Role `json:"role"`
	Disabled    *bool       `json:"disabled"`
	Language    *string     `json:"language"`
}

func (s *Server) handleUpdateUser(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	var req updateUserReq
	if err := decode(r, &req); err != nil {
		s.writeErr(w, r, err)
		return
	}
	u, err := s.store.GetUser(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	newRole := u.Role
	if req.Role != nil {
		if !req.Role.Valid() {
			s.writeErr(w, r, errValidation(map[string]string{"role": "Invalid role."}))
			return
		}
		newRole = *req.Role
	}
	if !auth.CanManageUser(p.User.Role, u.Role, newRole) {
		s.writeErr(w, r, errForbidden)
		return
	}
	if u.ID == p.User.ID && (newRole != u.Role || (req.Disabled != nil && *req.Disabled)) {
		s.writeErr(w, r, &APIError{Status: 409, Code: "self_modification", Message: "You cannot change your own role or disable your own account."})
		return
	}
	disabling := req.Disabled != nil && *req.Disabled && !u.Disabled
	demoting := u.Role == model.RoleOwner && newRole != model.RoleOwner
	if (disabling || demoting) && u.Role == model.RoleOwner {
		n, err := s.store.CountActiveOwners(r.Context())
		if err != nil {
			s.writeErr(w, r, err)
			return
		}
		if n <= 1 {
			s.writeErr(w, r, &APIError{Status: 409, Code: "last_owner", Message: "At least one active owner must remain."})
			return
		}
	}
	changes := map[string]any{}
	if req.DisplayName != nil {
		if len(*req.DisplayName) > 80 {
			s.writeErr(w, r, errValidation(map[string]string{"display_name": "At most 80 characters."}))
			return
		}
		u.DisplayName = strings.TrimSpace(*req.DisplayName)
	}
	if req.Language != nil {
		if !validLanguage(*req.Language) {
			s.writeErr(w, r, errValidation(map[string]string{"language": "Must be en or fa."}))
			return
		}
		u.Language = *req.Language
	}
	if newRole != u.Role {
		changes["role"] = map[string]any{"from": u.Role, "to": newRole}
		u.Role = newRole
	}
	if req.Disabled != nil && *req.Disabled != u.Disabled {
		changes["disabled"] = *req.Disabled
		u.Disabled = *req.Disabled
	}
	if err := s.store.UpdateUser(r.Context(), u); err != nil {
		s.writeErr(w, r, err)
		return
	}
	if u.Disabled || changes["role"] != nil {
		_ = s.store.DeleteUserSessions(r.Context(), u.ID) // force re-authentication with new privileges
	}
	action := "user.update"
	if _, ok := changes["role"]; ok {
		action = "user.role_change"
	}
	if v, ok := changes["disabled"]; ok {
		if v.(bool) {
			action = "user.disable"
		} else {
			action = "user.enable"
		}
	}
	s.audit.Record(r.Context(), p.actor(), action, "user", u.ID, "", changes)
	writeJSON(w, 200, map[string]any{"user": u})
}

type resetPasswordReq struct {
	NewPassword string `json:"new_password"`
}

func (s *Server) handleResetUserPassword(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	var req resetPasswordReq
	if err := decode(r, &req); err != nil {
		s.writeErr(w, r, err)
		return
	}
	u, err := s.store.GetUser(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	if !auth.CanManageUser(p.User.Role, u.Role, u.Role) {
		s.writeErr(w, r, errForbidden)
		return
	}
	if err := security.ValidatePassword(req.NewPassword); err != nil {
		s.writeErr(w, r, errValidation(map[string]string{"new_password": err.Error()}))
		return
	}
	hash, err := security.HashPassword(req.NewPassword, s.argon)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	if err := s.store.SetPassword(r.Context(), u.ID, hash); err != nil {
		s.writeErr(w, r, err)
		return
	}
	s.audit.Record(r.Context(), p.actor(), "user.password_reset", "user", u.ID, "", nil)
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	u, err := s.store.GetUser(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	if u.ID == p.User.ID {
		s.writeErr(w, r, &APIError{Status: 409, Code: "self_modification", Message: "You cannot delete your own account."})
		return
	}
	if !auth.CanManageUser(p.User.Role, u.Role, u.Role) {
		s.writeErr(w, r, errForbidden)
		return
	}
	if u.Role == model.RoleOwner && !u.Disabled {
		n, err := s.store.CountActiveOwners(r.Context())
		if err != nil {
			s.writeErr(w, r, err)
			return
		}
		if n <= 1 {
			s.writeErr(w, r, &APIError{Status: 409, Code: "last_owner", Message: "At least one active owner must remain."})
			return
		}
	}
	if err := s.store.DeleteUser(r.Context(), u.ID); err != nil {
		s.writeErr(w, r, err)
		return
	}
	s.audit.Record(r.Context(), p.actor(), "user.delete", "user", u.ID, "", map[string]any{"username": u.Username})
	writeJSON(w, 200, map[string]any{"ok": true})
}
