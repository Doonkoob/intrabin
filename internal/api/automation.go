package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/intrabin/intrabin/internal/auth"
	"github.com/intrabin/intrabin/internal/automation"
	"github.com/intrabin/intrabin/internal/dns"
	"github.com/intrabin/intrabin/internal/model"
	"github.com/intrabin/intrabin/internal/storage"
)

// workflowInput is the create/update payload. Secret values are write-only.
type workflowInput struct {
	Name           string                `json:"name"`
	Description    string                `json:"description"`
	Enabled        *bool                 `json:"enabled"`
	OperatorCanRun bool                  `json:"operator_can_run"`
	Trigger        model.WorkflowTrigger `json:"trigger"`
	Steps          []model.WorkflowStep  `json:"steps"`
	// Secrets replaces the whole secret set when non-nil. A key whose value is empty keeps its
	// current value (so the UI can re-submit names without ever seeing values).
	Secrets      map[string]string `json:"secrets"`
	ClearSecrets bool              `json:"clear_secrets"`
}

var (
	envNameRe    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)
	ctxKeyRe     = regexp.MustCompile(`^[a-z][a-z0-9_]{0,39}$`)
	reservedEnvs = []string{"PATH", "HOME", "LANG", "TMPDIR", "SHELL", "IFS", "PYTHONPATH", "PYTHONHOME", "BASH_ENV", "ENV"}
	httpMethods  = []string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD"}
	stepStatuses = []model.IncidentStatus{model.IncidentAcknowledged, model.IncidentRecoveryPending, model.IncidentRecovering,
		model.IncidentFailedOver, model.IncidentResolved, model.IncidentManualIntervention}
	triggerStatuses = []model.IncidentStatus{model.IncidentSuspected, model.IncidentConfirmed, model.IncidentAcknowledged, model.IncidentRecoveryPending,
		model.IncidentRecovering, model.IncidentFailedOver, model.IncidentResolved, model.IncidentManualIntervention}
	placeholderRe = regexp.MustCompile(`\{\{[^}]*\}\}`)
	statusCodeRe  = regexp.MustCompile(`^([1-5]xx|[1-5][0-9][0-9])$`)
)

func validEnvName(k string) bool {
	return envNameRe.MatchString(k) && !strings.HasPrefix(strings.ToUpper(k), "LD_") && !strings.HasPrefix(strings.ToUpper(k), "INTRABIN_") &&
		!slices.Contains(reservedEnvs, strings.ToUpper(k))
}

func (s *Server) normalizeWorkflow(r *http.Request, in *workflowInput) map[string]string {
	f := map[string]string{}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" || len(in.Name) > 100 {
		f["name"] = "Name is required (at most 100 characters)."
	}
	if len(in.Description) > 2000 {
		f["description"] = "Description is too long."
	}
	t := &in.Trigger
	switch t.Type {
	case "manual":
		*t = model.WorkflowTrigger{Type: "manual"}
	case "incident":
		t.IntervalSeconds = 0
		if len(t.OnStatuses) == 0 {
			t.OnStatuses = []model.IncidentStatus{model.IncidentConfirmed}
		}
		for _, st := range t.OnStatuses {
			if !slices.Contains(triggerStatuses, st) {
				f["trigger.on_statuses"] = "Unknown incident status " + string(st) + "."
			}
		}
		for _, id := range t.MonitorIDs {
			if _, err := s.store.GetMonitor(r.Context(), id); err != nil {
				f["trigger.monitor_ids"] = "Unknown monitor."
			}
		}
		if len(t.GroupIDs) > 50 || len(t.Tags) > 50 || len(t.MonitorIDs) > 200 {
			f["trigger"] = "Too many filters."
		}
	case "schedule":
		*t = model.WorkflowTrigger{Type: "schedule", IntervalSeconds: t.IntervalSeconds}
		if t.IntervalSeconds < 60 || t.IntervalSeconds > 30*86400 {
			f["trigger.interval_seconds"] = "Interval must be between 60 seconds and 30 days."
		}
	default:
		f["trigger.type"] = "Trigger must be manual, incident or schedule."
	}
	if len(in.Steps) == 0 || len(in.Steps) > 50 {
		f["steps"] = "A workflow needs between 1 and 50 steps."
	}
	for i := range in.Steps {
		st := &in.Steps[i]
		k := fmt.Sprintf("steps.%d.", i)
		st.Name = strings.TrimSpace(st.Name)
		if st.Name == "" {
			st.Name = fmt.Sprintf("Step %d", i+1)
		}
		if len(st.Name) > 100 {
			f[k+"name"] = "Name is too long."
		}
		if st.OnFailure == "" {
			st.OnFailure = "stop"
		}
		if st.OnFailure != "stop" && st.OnFailure != "continue" {
			f[k+"on_failure"] = "Must be stop or continue."
		}
		if st.Retries < 0 || st.Retries > 5 {
			f[k+"retries"] = "Retries must be between 0 and 5."
		}
		switch st.Type {
		case "script":
			*st = model.WorkflowStep{Name: st.Name, Type: st.Type, TimeoutSeconds: st.TimeoutSeconds, OnFailure: st.OnFailure, Retries: st.Retries,
				Language: st.Language, Script: st.Script, Env: st.Env}
			if st.TimeoutSeconds == 0 {
				st.TimeoutSeconds = 60
			}
			if st.TimeoutSeconds < 1 || st.TimeoutSeconds > 3600 {
				f[k+"timeout_seconds"] = "Timeout must be between 1 and 3600 seconds."
			}
			if st.Language != "bash" && st.Language != "python" {
				f[k+"language"] = "Language must be bash or python."
			}
			if strings.TrimSpace(st.Script) == "" || len(st.Script) > 256<<10 {
				f[k+"script"] = "Script is required (at most 256 KiB)."
			}
			if len(st.Env) > 50 {
				f[k+"env"] = "Too many environment variables."
			}
			for name, v := range st.Env {
				if !validEnvName(name) || len(v) > 8192 {
					f[k+"env"] = "Invalid or reserved environment variable " + name + "."
				}
			}
		case "http":
			*st = model.WorkflowStep{Name: st.Name, Type: st.Type, TimeoutSeconds: st.TimeoutSeconds, OnFailure: st.OnFailure, Retries: st.Retries,
				Method: strings.ToUpper(st.Method), URL: strings.TrimSpace(st.URL), Headers: st.Headers, Body: st.Body, ExpectedStatus: st.ExpectedStatus,
				AllowPrivateNetwork: st.AllowPrivateNetwork}
			if st.Method == "" {
				st.Method = "POST"
			}
			if !slices.Contains(httpMethods, st.Method) {
				f[k+"method"] = "Unsupported method."
			}
			if st.TimeoutSeconds == 0 {
				st.TimeoutSeconds = 30
			}
			if st.TimeoutSeconds < 1 || st.TimeoutSeconds > 300 {
				f[k+"timeout_seconds"] = "Timeout must be between 1 and 300 seconds."
			}
			_, rest, _ := strings.Cut(st.URL, "://")
			authority := rest
			if i := strings.IndexAny(rest, "/?#"); i >= 0 {
				authority = rest[:i]
			}
			u, err := url.Parse(placeholderRe.ReplaceAllString(st.URL, "x"))
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || strings.Contains(authority, "{{") {
				f[k+"url"] = "Enter an http(s) URL. Placeholders are allowed in the path and query only."
			}
			for h, v := range st.Headers {
				if !headerNameRe.MatchString(h) || strings.EqualFold(h, "Host") || strings.ContainsAny(v, "\r\n") {
					f[k+"headers"] = "Invalid header " + h + "."
				}
			}
			if len(st.Body) > 64<<10 {
				f[k+"body"] = "Body is too large (64 KiB maximum)."
			}
			if len(st.ExpectedStatus) == 0 {
				st.ExpectedStatus = []string{"2xx"}
			}
			for _, c := range st.ExpectedStatus {
				if !statusCodeRe.MatchString(strings.ToLower(c)) {
					f[k+"expected_status"] = "Use codes such as 200 or 2xx."
				}
			}
		case "wait":
			*st = model.WorkflowStep{Name: st.Name, Type: st.Type, OnFailure: "stop", Seconds: st.Seconds}
			if st.Seconds < 1 || st.Seconds > 86400 {
				f[k+"seconds"] = "Wait between 1 second and 24 hours."
			}
		case "incident_status":
			*st = model.WorkflowStep{Name: st.Name, Type: st.Type, OnFailure: st.OnFailure, Status: st.Status, Note: st.Note}
			if !slices.Contains(stepStatuses, st.Status) {
				f[k+"status"] = "Choose a recovery or resolution status."
			}
			if len(st.Note) > 1000 {
				f[k+"note"] = "Note is too long."
			}
		case "dns":
			*st = model.WorkflowStep{Name: st.Name, Type: st.Type, OnFailure: st.OnFailure, Retries: st.Retries, ProviderID: st.ProviderID,
				ZoneID: strings.TrimSpace(st.ZoneID), ZoneName: strings.ToLower(strings.TrimSuffix(strings.TrimSpace(st.ZoneName), ".")), DNSAction: st.DNSAction,
				RecordType: strings.ToUpper(st.RecordType), RecordName: strings.TrimSpace(st.RecordName), Content: strings.TrimSpace(st.Content), TTL: st.TTL, Proxied: st.Proxied}
			if dp, err := s.store.GetDNSProvider(r.Context(), st.ProviderID); err != nil {
				f[k+"provider_id"] = "Choose a DNS integration."
			} else if dp.Type != "cloudflare" {
				st.Proxied = false
			}
			if st.ZoneName == "" && st.ZoneID == "" {
				f[k+"zone_name"] = "Choose a zone."
			}
			if st.DNSAction != "upsert" && st.DNSAction != "delete" {
				f[k+"dns_action"] = "Choose create/update or delete."
			}
			if !slices.Contains(dns.EditableTypes, st.RecordType) {
				f[k+"record_type"] = "Choose A, AAAA, CNAME or TXT."
			}
			if st.RecordName == "" || len(st.RecordName) > 253 {
				f[k+"record_name"] = "Enter the record name."
			}
			if st.TTL == 0 {
				st.TTL = 1
			}
			if st.DNSAction == "upsert" {
				if st.Content == "" || len(st.Content) > 4096 {
					f[k+"content"] = "Enter the record value."
				} else if !placeholderRe.MatchString(st.Content+st.RecordName) && st.ZoneName != "" {
					// Fully static: validate now so mistakes surface when saving, not during an incident.
					in := dns.RecordInput{Type: st.RecordType, Name: st.RecordName, Content: st.Content, TTL: st.TTL, Proxied: st.Proxied}
					if err := dns.Validate(&in, st.ZoneName); err != nil {
						f[k+"content"] = err.Error()
					}
				}
			} else {
				st.Content, st.TTL, st.Proxied = "", 0, false
			}
		default:
			f[k+"type"] = "Step type must be script, http, wait, incident_status or dns."
		}
	}
	if len(in.Secrets) > 20 {
		f["secrets"] = "At most 20 secrets."
	}
	for k, v := range in.Secrets {
		if !validEnvName(k) || len(v) > 8192 {
			f["secrets"] = "Invalid or reserved secret name " + k + "."
		}
	}
	return f
}

// applySecrets computes the new sealed secret set.
func (s *Server) applySecrets(wf *model.Workflow, in *workflowInput) error {
	if in.ClearSecrets {
		wf.SecretBlob, wf.SecretKeys = nil, []string{}
		return nil
	}
	if in.Secrets == nil {
		return nil
	}
	cur, err := automation.OpenSecrets(s.box, wf)
	if err != nil {
		return err
	}
	next := map[string]string{}
	for k, v := range in.Secrets {
		if v == "" {
			if old, ok := cur[k]; ok {
				next[k] = old
			}
			continue
		}
		next[k] = v
	}
	blob, err := automation.SealSecrets(s.box, wf.ID, next)
	if err != nil {
		return err
	}
	keys := make([]string, 0, len(next))
	for k := range next {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	wf.SecretBlob, wf.SecretKeys = blob, keys
	return nil
}

func (s *Server) handleListWorkflows(w http.ResponseWriter, r *http.Request) {
	wfs, err := s.store.ListWorkflows(r.Context())
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"workflows": wfs})
}

func (s *Server) handleGetWorkflow(w http.ResponseWriter, r *http.Request) {
	wf, err := s.userWorkflow(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	runs, err := s.store.ListRuns(r.Context(), storage.RunFilter{WorkflowID: wf.ID, Limit: 20})
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"workflow": wf, "runs": runs})
}

func (s *Server) handleCreateWorkflow(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	var in workflowInput
	if err := decode(r, &in); err != nil {
		s.writeErr(w, r, err)
		return
	}
	if f := s.normalizeWorkflow(r, &in); len(f) > 0 {
		s.writeErr(w, r, errValidation(f))
		return
	}
	now := time.Now().UTC()
	wf := &model.Workflow{ID: uuid.NewString(), Name: in.Name, Description: in.Description, Enabled: in.Enabled != nil && *in.Enabled,
		OperatorCanRun: in.OperatorCanRun, Trigger: in.Trigger, Steps: in.Steps, SecretKeys: []string{}, Version: 1, CreatedBy: p.User.Username, CreatedAt: now, UpdatedAt: now}
	if err := s.applySecrets(wf, &in); err != nil {
		s.writeErr(w, r, err)
		return
	}
	if err := s.cp.CreateWorkflow(r.Context(), wf); err != nil {
		s.writeErr(w, r, err)
		return
	}
	s.audit.Record(r.Context(), p.actor(), "workflow.create", "workflow", wf.ID, "", map[string]any{"name": wf.Name, "enabled": wf.Enabled, "trigger": wf.Trigger.Type, "secret_keys": wf.SecretKeys})
	writeJSON(w, 201, map[string]any{"workflow": wf})
}

func (s *Server) handleUpdateWorkflow(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	wf, err := s.userWorkflow(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	var in workflowInput
	if err := decode(r, &in); err != nil {
		s.writeErr(w, r, err)
		return
	}
	if f := s.normalizeWorkflow(r, &in); len(f) > 0 {
		s.writeErr(w, r, errValidation(f))
		return
	}
	wasEnabled := wf.Enabled
	if in.Enabled != nil {
		wf.Enabled = *in.Enabled
	}
	if wf.Trigger.Type != in.Trigger.Type || wf.Trigger.IntervalSeconds != in.Trigger.IntervalSeconds || !wf.Enabled {
		wf.NextFireAt = nil // re-arm the schedule from now
	}
	wf.Name, wf.Description, wf.OperatorCanRun, wf.Trigger, wf.Steps = in.Name, in.Description, in.OperatorCanRun, in.Trigger, in.Steps
	wf.Version++
	wf.UpdatedAt = time.Now().UTC()
	if err := s.applySecrets(wf, &in); err != nil {
		s.writeErr(w, r, err)
		return
	}
	if err := s.cp.UpdateWorkflow(r.Context(), wf); err != nil {
		s.writeErr(w, r, err)
		return
	}
	s.audit.Record(r.Context(), p.actor(), "workflow.update", "workflow", wf.ID, "", map[string]any{"name": wf.Name, "version": wf.Version,
		"enabled": wf.Enabled, "was_enabled": wasEnabled, "secret_keys": wf.SecretKeys})
	writeJSON(w, 200, map[string]any{"workflow": wf})
}

func (s *Server) handleDeleteWorkflow(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	id := r.PathValue("id")
	wf, err := s.userWorkflow(r.Context(), id)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	busy, err := s.store.ListRuns(r.Context(), storage.RunFilter{WorkflowID: id, Statuses: []string{model.RunPending, model.RunRunning, model.RunWaiting, model.RunUnknownOutcome}, Limit: 1})
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	if len(busy) > 0 {
		s.writeErr(w, r, &APIError{Status: http.StatusConflict, Code: "workflow_busy", Message: "This workflow has active runs or runs with an unknown outcome. Cancel or reconcile them first."})
		return
	}
	if err := s.cp.DeleteWorkflow(r.Context(), id); err != nil {
		s.writeErr(w, r, err)
		return
	}
	s.audit.Record(r.Context(), p.actor(), "workflow.delete", "workflow", id, "", map[string]any{"name": wf.Name})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRunWorkflow(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	wf, err := s.userWorkflow(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	if !auth.Can(p.User.Role, auth.PermAutomationManage) && !wf.OperatorCanRun {
		s.writeErr(w, r, errForbidden)
		return
	}
	var in struct {
		Context map[string]string `json:"context"`
	}
	if r.ContentLength != 0 {
		if err := decode(r, &in); err != nil {
			s.writeErr(w, r, err)
			return
		}
	}
	if len(in.Context) > 20 {
		s.writeErr(w, r, errValidation(map[string]string{"context": "At most 20 values."}))
		return
	}
	cx := map[string]string{}
	for k, v := range in.Context {
		if !ctxKeyRe.MatchString(k) || len(v) > 1024 || k == "trigger" || k == "workflow_name" {
			s.writeErr(w, r, errValidation(map[string]string{"context": "Invalid context key " + k + "."}))
			return
		}
		cx[k] = v
	}
	if id := cx["incident_id"]; id != "" {
		in, err := s.store.GetIncident(r.Context(), id)
		if err != nil {
			s.writeErr(w, r, errValidation(map[string]string{"context": "Unknown incident."}))
			return
		}
		cx["incident_status"], cx["incident_title"], cx["monitor_id"] = string(in.Status), in.Title, in.MonitorID
	}
	run, err := s.automation.TriggerManual(r.Context(), wf, p.User.Username, cx)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	s.audit.Record(r.Context(), p.actor(), "workflow.run", "workflow", wf.ID, "", map[string]any{"run_id": run.ID, "name": wf.Name})
	writeJSON(w, 202, map[string]any{"run": run})
}

func (s *Server) handleListRuns(w http.ResponseWriter, r *http.Request) {
	f := storage.RunFilter{WorkflowID: r.URL.Query().Get("workflow_id"), Limit: queryInt(r, "limit", 100)}
	if st := r.URL.Query().Get("status"); st != "" {
		f.Statuses = strings.Split(st, ",")
	}
	runs, err := s.store.ListRuns(r.Context(), f)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"runs": runs})
}

func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	run, err := s.store.GetRun(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	if wf, err := s.store.GetWorkflow(r.Context(), run.WorkflowID); err == nil {
		run.WorkflowName = wf.Name
	}
	actions, err := s.store.RunActions(r.Context(), run.ID)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"run": run, "actions": actions})
}

func (s *Server) automationErr(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, automation.ErrInvalidState) {
		s.writeErr(w, r, &APIError{Status: http.StatusConflict, Code: "invalid_state", Message: strings.TrimPrefix(err.Error(), automation.ErrInvalidState.Error()+": ")})
		return
	}
	s.writeErr(w, r, err)
}

func (s *Server) handleCancelRun(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !auth.Can(p.User.Role, auth.PermAutomationManage) {
		cur, err := s.store.GetRun(r.Context(), r.PathValue("id"))
		if err != nil {
			s.writeErr(w, r, err)
			return
		}
		if wf, err := s.store.GetWorkflow(r.Context(), cur.WorkflowID); err != nil || !wf.OperatorCanRun {
			s.writeErr(w, r, errForbidden)
			return
		}
	}
	run, err := s.automation.Cancel(r.Context(), r.PathValue("id"), p.User.Username)
	if err != nil {
		s.automationErr(w, r, err)
		return
	}
	s.audit.Record(r.Context(), p.actor(), "workflow_run.cancel", "workflow_run", run.ID, "", nil)
	writeJSON(w, 200, map[string]any{"run": run})
}

func (s *Server) handleReconcileAction(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	var in struct {
		Result string `json:"result"`
		Note   string `json:"note"`
	}
	if err := decode(r, &in); err != nil {
		s.writeErr(w, r, err)
		return
	}
	in.Note = strings.TrimSpace(in.Note)
	f := map[string]string{}
	if in.Result != "succeeded" && in.Result != "failed" {
		f["result"] = "Choose succeeded or failed."
	}
	if in.Note == "" || len(in.Note) > 1000 {
		f["note"] = "Explain how you verified the outcome (required)."
	}
	if len(f) > 0 {
		s.writeErr(w, r, errValidation(f))
		return
	}
	id := r.PathValue("id")
	run, err := s.automation.Reconcile(r.Context(), id, in.Result, p.User.Username, in.Note)
	if err != nil {
		s.automationErr(w, r, err)
		return
	}
	s.audit.Record(r.Context(), p.actor(), "workflow_action.reconcile", "workflow_action", id, "", map[string]any{"run_id": run.ID, "result": in.Result, "note": in.Note})
	writeJSON(w, 200, map[string]any{"run": run})
}

func (s *Server) handleAutomationStatus(w http.ResponseWriter, r *http.Request) {
	ok, health, reason := s.automation.RunnerHealth(r.Context())
	interp := map[string]any{}
	if h, _ := health["interpreters"].(map[string]any); h != nil {
		for k := range h {
			interp[k] = true // report availability only, not paths
		}
	}
	counts := map[string]int{}
	for _, st := range []string{model.RunPending, model.RunRunning, model.RunWaiting, model.RunUnknownOutcome} {
		runs, err := s.store.ListRuns(r.Context(), storage.RunFilter{Statuses: []string{st}, Limit: 500})
		if err == nil {
			counts[st] = len(runs)
		}
	}
	writeJSON(w, 200, map[string]any{
		"executes_here": s.automation.IsActive(),
		"runner":        map[string]any{"configured": reason != "not configured", "reachable": ok, "interpreters": interp},
		"runs":          counts,
		"node_id":       s.node.Identity.ID,
		"checked_at":    strconv.FormatInt(time.Now().Unix(), 10),
	})
}

// userWorkflow loads a user-defined workflow; the backing rows of DR plans are not workflows here.
func (s *Server) userWorkflow(ctx context.Context, id string) (*model.Workflow, error) {
	wf, err := s.store.GetWorkflow(ctx, id)
	if err == nil && wf.Kind == "dr" {
		return nil, storage.ErrNotFound
	}
	return wf, err
}
