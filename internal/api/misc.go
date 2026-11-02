package api

import (
	_ "embed"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/intrabin/intrabin/internal/model"
	"github.com/intrabin/intrabin/internal/node"
	"github.com/intrabin/intrabin/internal/storage"
)

//go:embed openapi.yaml
var openapiSpec []byte

func (s *Server) handleOpenAPI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/yaml")
	_, _ = w.Write(openapiSpec)
}

func (s *Server) nodeInfo() map[string]any {
	return map[string]any{
		"node_id":    s.node.Identity.ID,
		"node_name":  s.node.Identity.Name,
		"role":       s.node.Role(),
		"can_manage": s.node.CanManage(),
		"version":    node.Version,
		"commit":     node.Commit,
		"build_date": node.BuildDate,
		"started_at": s.node.StartedAt,
	}
}

func (s *Server) handleNode(w http.ResponseWriter, r *http.Request) {
	info := s.nodeInfo()
	info["uptime_seconds"] = int(time.Since(s.node.StartedAt).Seconds())
	info["database_bytes"] = s.store.SizeBytes()
	ms, err := s.store.ListMonitors(r.Context())
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	assigned := 0
	for _, m := range ms {
		if m.Enabled && (len(m.NodeSelector) == 0 || contains(m.NodeSelector, s.node.Identity.ID)) {
			assigned++
		}
	}
	info["assigned_monitors"] = assigned
	writeJSON(w, 200, map[string]any{"node": info})
}

// ---- groups ----

type groupReq struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

func (g *groupReq) validate() map[string]string {
	g.Name = strings.TrimSpace(g.Name)
	f := map[string]string{}
	if g.Name == "" || len(g.Name) > 80 {
		f["name"] = "Name is required (max 80 characters)."
	}
	if len(g.Description) > 500 {
		f["description"] = "At most 500 characters."
	}
	return f
}

func (s *Server) handleListGroups(w http.ResponseWriter, r *http.Request) {
	gs, err := s.store.ListGroups(r.Context())
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	ms, err := s.store.ListMonitors(r.Context())
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	counts := map[string]int{}
	for _, m := range ms {
		counts[m.GroupID]++
	}
	type gv struct {
		*model.MonitorGroup
		MonitorCount int `json:"monitor_count"`
	}
	out := []gv{}
	for _, g := range gs {
		out = append(out, gv{g, counts[g.ID]})
	}
	writeJSON(w, 200, map[string]any{"groups": out})
}

func (s *Server) handleCreateGroup(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	var req groupReq
	if err := decode(r, &req); err != nil {
		s.writeErr(w, r, err)
		return
	}
	if f := req.validate(); len(f) > 0 {
		s.writeErr(w, r, errValidation(f))
		return
	}
	now := time.Now().UTC()
	g := &model.MonitorGroup{ID: uuid.NewString(), Name: req.Name, Description: req.Description, CreatedAt: now, UpdatedAt: now}
	if err := s.cp.CreateGroup(r.Context(), g); err != nil {
		s.writeErr(w, r, err)
		return
	}
	s.audit.Record(r.Context(), p.actor(), "monitor_group.create", "monitor_group", g.ID, "", map[string]any{"name": g.Name})
	writeJSON(w, 201, map[string]any{"group": g})
}

func (s *Server) handleUpdateGroup(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	var req groupReq
	if err := decode(r, &req); err != nil {
		s.writeErr(w, r, err)
		return
	}
	if f := req.validate(); len(f) > 0 {
		s.writeErr(w, r, errValidation(f))
		return
	}
	g, err := s.store.GetGroup(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	g.Name, g.Description, g.UpdatedAt = req.Name, req.Description, time.Now().UTC()
	if err := s.cp.UpdateGroup(r.Context(), g); err != nil {
		s.writeErr(w, r, err)
		return
	}
	s.audit.Record(r.Context(), p.actor(), "monitor_group.update", "monitor_group", g.ID, "", map[string]any{"name": g.Name})
	writeJSON(w, 200, map[string]any{"group": g})
}

func (s *Server) handleDeleteGroup(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	id := r.PathValue("id")
	if err := s.cp.DeleteGroup(r.Context(), id); err != nil {
		s.writeErr(w, r, err)
		return
	}
	s.engine.Reload()
	s.audit.Record(r.Context(), p.actor(), "monitor_group.delete", "monitor_group", id, "", nil)
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ---- incidents ----

func (s *Server) handleListIncidents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := storage.IncidentFilter{MonitorID: q.Get("monitor_id"), OpenOnly: q.Get("open") == "true", Limit: queryInt(r, "limit", 100)}
	if st := q.Get("status"); st != "" {
		f.Statuses = strings.Split(st, ",")
	}
	before, err := queryTime(r, "before")
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	f.Before = before
	incs, err := s.store.ListIncidents(r.Context(), f)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"incidents": incs})
}

func (s *Server) handleGetIncident(w http.ResponseWriter, r *http.Request) {
	in, err := s.store.GetIncident(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	evs, err := s.store.ListIncidentEvents(r.Context(), in.ID)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"incident": in, "events": evs, "node_names": s.nodeNames()})
}

type noteReq struct {
	Note string `json:"note"`
}

func (s *Server) incidentAction(w http.ResponseWriter, r *http.Request, action string) {
	p := principal(r)
	var req noteReq
	if r.ContentLength != 0 {
		if err := decode(r, &req); err != nil {
			s.writeErr(w, r, err)
			return
		}
	}
	req.Note = strings.TrimSpace(req.Note)
	if len(req.Note) > 2000 {
		s.writeErr(w, r, errValidation(map[string]string{"note": "At most 2000 characters."}))
		return
	}
	id := r.PathValue("id")
	var err error
	switch action {
	case "acknowledge":
		_, err = s.incidents.Acknowledge(r.Context(), id, p.User.Username, req.Note)
	case "resolve":
		_, err = s.incidents.Resolve(r.Context(), id, p.User.Username, req.Note)
	case "note":
		if req.Note == "" {
			s.writeErr(w, r, errValidation(map[string]string{"note": "Note is required."}))
			return
		}
		err = s.incidents.AddNote(r.Context(), id, p.User.Username, req.Note)
	}
	if err != nil {
		if strings.Contains(err.Error(), "invalid incident transition") {
			s.writeErr(w, r, &APIError{Status: 409, Code: "invalid_transition", Message: err.Error()})
			return
		}
		s.writeErr(w, r, err)
		return
	}
	s.audit.Record(r.Context(), p.actor(), "incident."+action, "incident", id, "", nil)
	s.handleGetIncident(w, r)
}

func (s *Server) handleAckIncident(w http.ResponseWriter, r *http.Request) {
	s.incidentAction(w, r, "acknowledge")
}
func (s *Server) handleResolveIncident(w http.ResponseWriter, r *http.Request) {
	s.incidentAction(w, r, "resolve")
}
func (s *Server) handleIncidentNote(w http.ResponseWriter, r *http.Request) {
	s.incidentAction(w, r, "note")
}

// ---- maintenance ----

type maintReq struct {
	MonitorID string    `json:"monitor_id"`
	Title     string    `json:"title"`
	StartsAt  time.Time `json:"starts_at"`
	EndsAt    time.Time `json:"ends_at"`
}

func (s *Server) handleListMaintenance(w http.ResponseWriter, r *http.Request) {
	ws, err := s.store.ListMaintenance(r.Context(), time.Now().Add(-7*24*time.Hour))
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"maintenance_windows": ws})
}

func (s *Server) handleCreateMaintenance(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	var req maintReq
	if err := decode(r, &req); err != nil {
		s.writeErr(w, r, err)
		return
	}
	f := map[string]string{}
	req.Title = strings.TrimSpace(req.Title)
	if req.Title == "" || len(req.Title) > 120 {
		f["title"] = "Title is required (max 120 characters)."
	}
	if req.StartsAt.IsZero() || req.EndsAt.IsZero() || !req.EndsAt.After(req.StartsAt) {
		f["ends_at"] = "End must be after start."
	} else if req.EndsAt.Sub(req.StartsAt) > 31*24*time.Hour {
		f["ends_at"] = "Maintenance windows are limited to 31 days."
	}
	if req.MonitorID != "" {
		if _, err := s.store.GetMonitor(r.Context(), req.MonitorID); err != nil {
			f["monitor_id"] = "Unknown monitor."
		}
	}
	if len(f) > 0 {
		s.writeErr(w, r, errValidation(f))
		return
	}
	mw := &model.MaintenanceWindow{ID: uuid.NewString(), MonitorID: req.MonitorID, Title: req.Title, StartsAt: req.StartsAt.UTC(), EndsAt: req.EndsAt.UTC(), CreatedBy: p.User.Username, CreatedAt: time.Now().UTC()}
	if err := s.cp.CreateMaintenance(r.Context(), mw); err != nil {
		s.writeErr(w, r, err)
		return
	}
	s.audit.Record(r.Context(), p.actor(), "maintenance.create", "maintenance_window", mw.ID, "", map[string]any{"monitor_id": mw.MonitorID, "starts_at": mw.StartsAt, "ends_at": mw.EndsAt})
	writeJSON(w, 201, map[string]any{"maintenance_window": mw})
}

func (s *Server) handleDeleteMaintenance(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	id := r.PathValue("id")
	if err := s.cp.DeleteMaintenance(r.Context(), id); err != nil {
		s.writeErr(w, r, err)
		return
	}
	s.audit.Record(r.Context(), p.actor(), "maintenance.delete", "maintenance_window", id, "", nil)
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ---- audit ----

func (s *Server) handleListAudit(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := storage.AuditFilter{Action: q.Get("action"), Actor: q.Get("actor"), Target: q.Get("target_id"), Query: q.Get("q"), Limit: queryInt(r, "limit", 100)}
	var err error
	if f.Since, err = queryTime(r, "since"); err != nil {
		s.writeErr(w, r, err)
		return
	}
	if f.Before, err = queryTime(r, "before"); err != nil {
		s.writeErr(w, r, err)
		return
	}
	es, err := s.store.ListAudit(r.Context(), f)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"entries": es})
}

// ---- settings ----

type Settings struct {
	InstanceName      string `json:"instance_name"`
	DefaultLanguage   string `json:"default_language"`
	RawRetentionDays  int    `json:"raw_retention_days"`
	RoundRetentionDay int    `json:"round_retention_days"`
	HourlyRetention   int    `json:"hourly_retention_days"`
}

func (s *Server) loadSettings(r *http.Request) Settings {
	st := Settings{InstanceName: "Intrabin", DefaultLanguage: "en", RawRetentionDays: 14, RoundRetentionDay: 90, HourlyRetention: 400}
	all, _ := s.store.AllSettings(r.Context(), "")
	if v := all["app.instance_name"]; v != "" {
		st.InstanceName = v
	}
	if v := all["app.default_language"]; v != "" {
		st.DefaultLanguage = v
	}
	atoi := func(k string, d int) int {
		if n, err := strconv.Atoi(all[k]); err == nil && n > 0 {
			return n
		}
		return d
	}
	st.RawRetentionDays = atoi("retention.raw_days", st.RawRetentionDays)
	st.RoundRetentionDay = atoi("retention.rounds_days", st.RoundRetentionDay)
	st.HourlyRetention = atoi("retention.hourly_days", st.HourlyRetention)
	return st
}

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"settings": s.loadSettings(r)})
}

type settingsPatch struct {
	InstanceName       *string `json:"instance_name"`
	DefaultLanguage    *string `json:"default_language"`
	RawRetentionDays   *int    `json:"raw_retention_days"`
	RoundRetentionDays *int    `json:"round_retention_days"`
	HourlyRetention    *int    `json:"hourly_retention_days"`
}

func (s *Server) handleUpdateSettings(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	var req settingsPatch
	if err := decode(r, &req); err != nil {
		s.writeErr(w, r, err)
		return
	}
	f := map[string]string{}
	set := map[string]string{}
	if req.InstanceName != nil {
		v := strings.TrimSpace(*req.InstanceName)
		if v == "" || len(v) > 80 {
			f["instance_name"] = "1–80 characters."
		}
		set["app.instance_name"] = v
	}
	if req.DefaultLanguage != nil {
		if !validLanguage(*req.DefaultLanguage) {
			f["default_language"] = "Must be en or fa."
		}
		set["app.default_language"] = *req.DefaultLanguage
	}
	checkDays := func(field, key string, v *int, lo, hi int) {
		if v == nil {
			return
		}
		if *v < lo || *v > hi {
			f[field] = "Must be between " + strconv.Itoa(lo) + " and " + strconv.Itoa(hi) + " days."
		}
		set[key] = strconv.Itoa(*v)
	}
	checkDays("raw_retention_days", "retention.raw_days", req.RawRetentionDays, 1, 90)
	checkDays("round_retention_days", "retention.rounds_days", req.RoundRetentionDays, 7, 400)
	checkDays("hourly_retention_days", "retention.hourly_days", req.HourlyRetention, 30, 1825)
	if len(f) > 0 {
		s.writeErr(w, r, errValidation(f))
		return
	}
	for k, v := range set {
		if err := s.cp.SetSetting(r.Context(), k, v); err != nil {
			s.writeErr(w, r, err)
			return
		}
	}
	changed := map[string]any{}
	for k, v := range set {
		changed[k] = v
	}
	s.audit.Record(r.Context(), p.actor(), "settings.update", "settings", "", "", changed)
	writeJSON(w, 200, map[string]any{"settings": s.loadSettings(r)})
}

// ---- overview ----

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ms, err := s.store.ListMonitors(ctx)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	states, err := s.store.ListMonitorStates(ctx)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	counts := map[string]int{}
	var uptimeSum float64
	uptimeN := 0
	now := time.Now().UTC()
	type mini struct {
		ID        string              `json:"id"`
		Name      string              `json:"name"`
		Status    model.MonitorStatus `json:"status"`
		Uptime24h *float64            `json:"uptime_24h"`
		LatencyMs *float64            `json:"latency_ms"`
	}
	list := []mini{}
	for _, m := range ms {
		st := states[m.ID]
		status := model.StatusUnknown
		var lat *float64
		if st != nil {
			status = st.Status
			lat = st.LastLatencyMs
		}
		counts[string(status)]++
		u, err := s.store.Uptime(ctx, m.ID, now.Add(-24*time.Hour))
		if err != nil {
			s.writeErr(w, r, err)
			return
		}
		if u.Availability != nil {
			uptimeSum += *u.Availability
			uptimeN++
		}
		list = append(list, mini{ID: m.ID, Name: m.Name, Status: status, Uptime24h: u.Availability, LatencyMs: lat})
	}
	var avgUptime *float64
	if uptimeN > 0 {
		v := uptimeSum / float64(uptimeN)
		avgUptime = &v
	}
	open, err := s.store.ListIncidents(ctx, storage.IncidentFilter{OpenOnly: true, Limit: 50})
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	recent, err := s.store.ListIncidents(ctx, storage.IncidentFilter{Limit: 8})
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	latency, err := s.store.GlobalLatencySeries(ctx, now.Add(-24*time.Hour), 15*time.Minute)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	health, err := s.store.GlobalDailyUptime(ctx, 30, now)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{
		"monitors":       map[string]any{"total": len(ms), "by_status": counts, "list": list},
		"incidents":      map[string]any{"open": len(open), "open_list": open, "recent": recent},
		"uptime_24h_avg": avgUptime,
		"latency_24h":    latency,
		"health_30d":     health,
		"node":           s.nodeInfo(),
		"cluster":        map[string]any{"mode": "standalone", "nodes_total": 1, "nodes_online": 1, "leader": s.node.Identity.Name, "quorum": "not_applicable"},
	})
}
