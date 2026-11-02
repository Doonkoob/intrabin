package api

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/intrabin/intrabin/internal/dns"
	"github.com/intrabin/intrabin/internal/dr"
	"github.com/intrabin/intrabin/internal/model"
	"github.com/intrabin/intrabin/internal/storage"
)

type drPlanInput struct {
	Name         string            `json:"name"`
	Description  string            `json:"description"`
	Config       model.DRConfig    `json:"config"`
	Secrets      map[string]string `json:"secrets"`
	ClearSecrets bool              `json:"clear_secrets"`
}

var drRecordTypes = []string{"A", "AAAA", "CNAME"}

// normalizeDRPlan validates a plan; the pre/post steps reuse the workflow step validation.
func (s *Server) normalizeDRPlan(r *http.Request, in *drPlanInput) map[string]string {
	f := map[string]string{}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" || len(in.Name) > 100 {
		f["name"] = "Name is required (at most 100 characters)."
	}
	if len(in.Description) > 2000 {
		f["description"] = "Description is too long."
	}
	c := &in.Config
	if len(c.MonitorIDs) == 0 || len(c.MonitorIDs) > 20 {
		f["config.monitor_ids"] = "Choose the monitors that watch the primary (1 to 20)."
	}
	seen := map[string]bool{}
	for _, id := range c.MonitorIDs {
		if _, err := s.store.GetMonitor(r.Context(), id); err != nil || seen[id] {
			f["config.monitor_ids"] = "Unknown or repeated monitor."
		}
		seen[id] = true
	}
	if c.SecondaryMonitorID != "" {
		if _, err := s.store.GetMonitor(r.Context(), c.SecondaryMonitorID); err != nil {
			f["config.secondary_monitor_id"] = "Unknown monitor."
		} else if seen[c.SecondaryMonitorID] {
			f["config.secondary_monitor_id"] = "The secondary's monitor cannot also watch the primary."
		}
	}
	if c.Automatic && c.SecondaryMonitorID == "" {
		f["config.secondary_monitor_id"] = "Automatic failover needs a monitor for the secondary: it only fails over to a secondary that is UP."
	}
	if c.CooldownSeconds == 0 {
		c.CooldownSeconds = int(dr.DefaultCooldown / time.Second)
	}
	if c.CooldownSeconds < 60 || c.CooldownSeconds > 7*86400 {
		f["config.cooldown_seconds"] = "Cooldown must be between 1 minute and 7 days."
	}
	c.RecordType = strings.ToUpper(c.RecordType)
	c.ZoneName = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(c.ZoneName), "."))
	c.RecordName = strings.TrimSpace(c.RecordName)
	c.PrimaryValue, c.SecondaryValue = strings.TrimSpace(c.PrimaryValue), strings.TrimSpace(c.SecondaryValue)
	if c.TTL == 0 {
		c.TTL = 60
	}
	dp, err := s.store.GetDNSProvider(r.Context(), c.ProviderID)
	if err != nil {
		f["config.provider_id"] = "Choose a DNS integration."
	} else if dp.Type != "cloudflare" {
		c.Proxied = false
	}
	if c.ZoneName == "" {
		f["config.zone_name"] = "Enter the zone."
	}
	if !slices.Contains(drRecordTypes, c.RecordType) {
		f["config.record_type"] = "Choose A, AAAA or CNAME."
	}
	if c.RecordName == "" {
		f["config.record_name"] = "Enter the record name."
	}
	for k, v := range map[string]*string{"config.primary_value": &c.PrimaryValue, "config.secondary_value": &c.SecondaryValue} {
		if *v == "" {
			f[k] = "Enter the value."
			continue
		}
		if c.ZoneName != "" && f["config.record_type"] == "" {
			ri := dns.RecordInput{Type: c.RecordType, Name: c.RecordName, Content: *v, TTL: c.TTL, Proxied: c.Proxied}
			if err := dns.Validate(&ri, c.ZoneName); err != nil {
				f[k] = err.Error()
			} else {
				*v = ri.Content
				c.RecordName = ri.Name
			}
		}
	}
	if c.PrimaryValue != "" && c.PrimaryValue == c.SecondaryValue {
		f["config.secondary_value"] = "The secondary value must differ from the primary value."
	}
	// Pre/post steps: script, http or wait, validated like workflow steps.
	steps := append(append([]model.WorkflowStep{}, c.PreSteps...), c.PostSteps...)
	for i, st := range steps {
		if st.Type != "script" && st.Type != "http" && st.Type != "wait" {
			f[stepKey(i, len(c.PreSteps), "type")] = "Only script, HTTP and wait steps can run around the DNS switch."
		}
	}
	if len(steps) > 20 {
		f["config.pre_steps"] = "At most 20 extra steps."
	}
	wi := workflowInput{Name: "dr", Trigger: model.WorkflowTrigger{Type: "manual"}, Steps: steps, Secrets: in.Secrets}
	for k, v := range s.normalizeWorkflow(r, &wi) {
		switch {
		case k == "steps":
		case strings.HasPrefix(k, "steps."):
			var i int
			var rest string
			fmt.Sscanf(strings.TrimPrefix(k, "steps."), "%d.%s", &i, &rest)
			f[stepKey(i, len(c.PreSteps), rest)] = v
		default:
			f[k] = v
		}
	}
	c.PreSteps, c.PostSteps = wi.Steps[:len(c.PreSteps)], wi.Steps[len(c.PreSteps):]
	if c.MonitorIDs == nil {
		c.MonitorIDs = []string{}
	}
	return f
}

func stepKey(i, nPre int, field string) string {
	if i < nPre {
		return fmt.Sprintf("config.pre_steps.%d.%s", i, field)
	}
	return fmt.Sprintf("config.post_steps.%d.%s", i-nPre, field)
}

type drPlanView struct {
	*model.DRPlan
	Waiting   string `json:"waiting,omitempty"`
	RunStatus string `json:"run_status,omitempty"`
}

func (s *Server) drView(r *http.Request, p *model.DRPlan) drPlanView {
	v := drPlanView{DRPlan: p}
	if s.dr != nil {
		v.Waiting = s.dr.WaitingReason(p.ID)
	}
	if p.ActiveRunID != "" {
		if run, err := s.store.GetRun(r.Context(), p.ActiveRunID); err == nil {
			v.RunStatus = run.Status
		}
	}
	return v
}

func (s *Server) handleListDRPlans(w http.ResponseWriter, r *http.Request) {
	ps, err := s.store.ListDRPlans(r.Context())
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	out := make([]drPlanView, 0, len(ps))
	for _, p := range ps {
		out = append(out, s.drView(r, p))
	}
	writeJSON(w, 200, map[string]any{"plans": out})
}

func (s *Server) handleGetDRPlan(w http.ResponseWriter, r *http.Request) {
	p, err := s.store.GetDRPlan(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	runs, err := s.store.ListRuns(r.Context(), storage.RunFilter{WorkflowID: p.ID, Limit: 20})
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"plan": s.drView(r, p), "runs": runs})
}

func (s *Server) handleCreateDRPlan(w http.ResponseWriter, r *http.Request) {
	pr := principal(r)
	var in drPlanInput
	if err := decode(r, &in); err != nil {
		s.writeErr(w, r, err)
		return
	}
	if f := s.normalizeDRPlan(r, &in); len(f) > 0 {
		s.writeErr(w, r, errValidation(f))
		return
	}
	now := time.Now().UTC()
	wf := &model.Workflow{ID: uuid.NewString(), Kind: "dr", Name: in.Name, Description: in.Description, Trigger: model.WorkflowTrigger{Type: "manual"},
		Steps: []model.WorkflowStep{}, SecretKeys: []string{}, Version: 1, CreatedBy: pr.User.Username, CreatedAt: now, UpdatedAt: now}
	if err := s.applySecrets(wf, &workflowInput{Secrets: in.Secrets, ClearSecrets: in.ClearSecrets}); err != nil {
		s.writeErr(w, r, err)
		return
	}
	p := &model.DRPlan{ID: wf.ID, Config: in.Config, State: model.DRPrimary, LastTransitionAt: now, UpdatedAt: now}
	if err := s.cp.SaveDRPlan(r.Context(), wf, p, true); err != nil {
		s.writeErr(w, r, err)
		return
	}
	s.audit.Record(r.Context(), pr.actor(), "dr_plan.create", "dr_plan", p.ID, "", map[string]any{"name": in.Name, "automatic": in.Config.Automatic, "record": in.Config.RecordName})
	got, _ := s.store.GetDRPlan(r.Context(), p.ID)
	writeJSON(w, 201, map[string]any{"plan": got})
}

func drBusy(p *model.DRPlan) bool {
	return p.State == model.DRFailingOver || p.State == model.DRFailingBack
}

var errDRBusy = &APIError{Status: http.StatusConflict, Code: "dr_busy", Message: "A failover or failback of this plan is in progress."}

func (s *Server) handleUpdateDRPlan(w http.ResponseWriter, r *http.Request) {
	pr := principal(r)
	cur, err := s.store.GetDRPlan(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	if drBusy(cur) {
		s.writeErr(w, r, errDRBusy)
		return
	}
	var in drPlanInput
	if err := decode(r, &in); err != nil {
		s.writeErr(w, r, err)
		return
	}
	if f := s.normalizeDRPlan(r, &in); len(f) > 0 {
		s.writeErr(w, r, errValidation(f))
		return
	}
	wf, err := s.store.GetWorkflow(r.Context(), cur.ID)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	wf.Name, wf.Description = in.Name, in.Description
	wf.Version++
	wf.UpdatedAt = time.Now().UTC()
	if err := s.applySecrets(wf, &workflowInput{Secrets: in.Secrets, ClearSecrets: in.ClearSecrets}); err != nil {
		s.writeErr(w, r, err)
		return
	}
	cur.Config, cur.UpdatedAt = in.Config, wf.UpdatedAt
	if err := s.cp.SaveDRPlan(r.Context(), wf, cur, false); err != nil {
		s.writeErr(w, r, err)
		return
	}
	s.audit.Record(r.Context(), pr.actor(), "dr_plan.update", "dr_plan", cur.ID, "", map[string]any{"name": in.Name, "automatic": in.Config.Automatic, "version": wf.Version})
	if s.dr != nil {
		s.dr.Kick()
	}
	got, _ := s.store.GetDRPlan(r.Context(), cur.ID)
	writeJSON(w, 200, map[string]any{"plan": got})
}

func (s *Server) handleDeleteDRPlan(w http.ResponseWriter, r *http.Request) {
	pr := principal(r)
	p, err := s.store.GetDRPlan(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	if drBusy(p) {
		s.writeErr(w, r, errDRBusy)
		return
	}
	if err := s.cp.DeleteWorkflow(r.Context(), p.ID); err != nil {
		s.writeErr(w, r, err)
		return
	}
	s.audit.Record(r.Context(), pr.actor(), "dr_plan.delete", "dr_plan", p.ID, "", map[string]any{"name": p.Name, "state": p.State})
	w.WriteHeader(204)
}

func (s *Server) drErr(w http.ResponseWriter, r *http.Request, res *dr.Result, err error) {
	switch {
	case errors.Is(err, dr.ErrBlocked):
		writeJSON(w, 409, map[string]any{"error": &APIError{Code: "dr_blocked", Message: "A safety check failed. Review the checks, fix the cause or force with a note where allowed."}, "result": res})
	case errors.Is(err, dr.ErrState):
		s.writeErr(w, r, &APIError{Status: 409, Code: "dr_state", Message: "The plan is not in a state that allows this (another switch may have started)."})
	default:
		s.writeErr(w, r, err)
	}
}

func (s *Server) handlePreviewDRPlan(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Direction string `json:"direction"`
	}
	if err := decode(r, &in); err != nil {
		s.writeErr(w, r, err)
		return
	}
	if in.Direction != dr.Failover && in.Direction != dr.Failback {
		s.writeErr(w, r, errValidation(map[string]string{"direction": "Choose failover or failback."}))
		return
	}
	res, err := s.dr.Preview(r.Context(), r.PathValue("id"), in.Direction, false)
	if err != nil {
		s.drErr(w, r, nil, err)
		return
	}
	writeJSON(w, 200, map[string]any{"result": res})
}

func (s *Server) handleDRSwitch(direction string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		pr := principal(r)
		var in struct {
			Note  string `json:"note"`
			Force bool   `json:"force"`
		}
		if err := decode(r, &in); err != nil {
			s.writeErr(w, r, err)
			return
		}
		in.Note = strings.TrimSpace(in.Note)
		if len(in.Note) > 1000 {
			s.writeErr(w, r, errValidation(map[string]string{"note": "Note is too long."}))
			return
		}
		if in.Force && in.Note == "" {
			s.writeErr(w, r, errValidation(map[string]string{"note": "Explain why you override the safety checks (required)."}))
			return
		}
		id := r.PathValue("id")
		res, err := s.dr.Start(r.Context(), id, direction, dr.StartOptions{Actor: pr.User.Username, Note: in.Note, Force: in.Force})
		meta := map[string]any{"direction": direction, "forced": in.Force, "note": in.Note}
		if res != nil {
			var failed []string
			for _, c := range res.Checks {
				if !c.OK {
					failed = append(failed, c.ID)
				}
			}
			meta["failed_checks"] = failed
		}
		if err != nil {
			meta["error"] = err.Error()
			s.audit.Record(r.Context(), pr.actor(), "dr_plan."+direction+"_refused", "dr_plan", id, "", meta)
			s.drErr(w, r, res, err)
			return
		}
		meta["run_id"] = res.Run.ID
		s.audit.Record(r.Context(), pr.actor(), "dr_plan."+direction, "dr_plan", id, "", meta)
		writeJSON(w, 200, map[string]any{"result": res})
	}
}

func (s *Server) handleResolveDRPlan(w http.ResponseWriter, r *http.Request) {
	pr := principal(r)
	var in struct {
		State string `json:"state"`
		Note  string `json:"note"`
	}
	if err := decode(r, &in); err != nil {
		s.writeErr(w, r, err)
		return
	}
	in.Note = strings.TrimSpace(in.Note)
	f := map[string]string{}
	if in.State != model.DRPrimary && in.State != model.DRFailedOver {
		f["state"] = "Choose where traffic goes now: primary or secondary."
	}
	if in.Note == "" || len(in.Note) > 1000 {
		f["note"] = "Explain how you verified this (required)."
	}
	if len(f) > 0 {
		s.writeErr(w, r, errValidation(f))
		return
	}
	p, err := s.dr.Resolve(r.Context(), r.PathValue("id"), in.State, pr.User.Username, in.Note)
	if err != nil {
		s.drErr(w, r, nil, err)
		return
	}
	s.audit.Record(r.Context(), pr.actor(), "dr_plan.resolve", "dr_plan", p.ID, "", map[string]any{"state": in.State, "note": in.Note})
	writeJSON(w, 200, map[string]any{"plan": p})
}
