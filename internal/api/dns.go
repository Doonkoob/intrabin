package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/intrabin/intrabin/internal/dns"
	"github.com/intrabin/intrabin/internal/dr"
	"github.com/intrabin/intrabin/internal/model"
	"github.com/intrabin/intrabin/internal/storage"
)

func (s *Server) dnsErr(w http.ResponseWriter, r *http.Request, err error) {
	var pe *dns.ProviderError
	var te *dns.TransportError
	switch {
	case errors.Is(err, dns.ErrConflict):
		s.writeErr(w, r, &APIError{Status: 409, Code: "dns_conflict", Message: "The record changed at the provider since this change was planned. Review the new plan and try again."})
	case errors.Is(err, dns.ErrNoChange):
		s.writeErr(w, r, &APIError{Status: 409, Code: "dns_no_change", Message: "The record already has this value."})
	case errors.Is(err, dns.ErrAmbiguous):
		s.writeErr(w, r, &APIError{Status: 422, Code: "dns_ambiguous", Message: "Several records with this name and type exist. Choose the record to change."})
	case errors.Is(err, dns.ErrInvalid), errors.Is(err, dns.ErrUnsupported):
		s.writeErr(w, r, &APIError{Status: 422, Code: "dns_invalid", Message: err.Error()})
	case errors.Is(err, dns.ErrAuth):
		s.writeErr(w, r, &APIError{Status: 422, Code: "dns_auth", Message: "The DNS provider rejected the credentials, or they lack permission for this operation."})
	case errors.Is(err, dns.ErrNotFound):
		s.writeErr(w, r, &APIError{Status: 404, Code: "dns_not_found", Message: dns.SafeError(err)})
	case errors.Is(err, dns.ErrRateLimited):
		s.writeErr(w, r, &APIError{Status: 503, Code: "dns_rate_limited", Message: "The DNS provider is rate-limiting requests. Try again shortly."})
	case errors.As(err, &pe), errors.As(err, &te):
		s.writeErr(w, r, &APIError{Status: 502, Code: "dns_provider_error", Message: dns.SafeError(err)})
	default:
		s.writeErr(w, r, err)
	}
}

type dnsProviderInput struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	APIToken string `json:"api_token"`
	APIKey   string `json:"api_key"`
}

func (s *Server) handleListDNSProviders(w http.ResponseWriter, r *http.Request) {
	ps, err := s.store.ListDNSProviders(r.Context())
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"providers": ps, "types": dns.ProviderTypes})
}

func (s *Server) verifyProvider(ctx context.Context, c dns.Provider) error {
	vctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	return c.Verify(vctx)
}

func (s *Server) handleCreateDNSProvider(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	var in dnsProviderInput
	if err := decode(r, &in); err != nil {
		s.writeErr(w, r, err)
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	f := map[string]string{}
	if in.Name == "" || len(in.Name) > 100 {
		f["name"] = "Name is required (at most 100 characters)."
	}
	creds := dns.Credentials{APIToken: strings.TrimSpace(in.APIToken), APIKey: strings.TrimSpace(in.APIKey)}
	switch in.Type {
	case "cloudflare":
		creds.APIKey = ""
		if creds.APIToken == "" || len(creds.APIToken) > 512 {
			f["api_token"] = "An API token is required."
		}
	case "arvancloud":
		creds.APIToken = ""
		if creds.APIKey == "" || len(creds.APIKey) > 512 {
			f["api_key"] = "An API key is required."
		}
	default:
		f["type"] = "Choose Cloudflare or ArvanCloud."
	}
	if len(f) > 0 {
		s.writeErr(w, r, errValidation(f))
		return
	}
	c, err := s.dns.NewClient(in.Type, creds)
	if err != nil {
		s.dnsErr(w, r, err)
		return
	}
	if err := s.verifyProvider(r.Context(), c); err != nil {
		s.dnsErr(w, r, err)
		return
	}
	now := time.Now().UTC()
	dp := &model.DNSProvider{ID: uuid.NewString(), Name: in.Name, Type: in.Type, CreatedBy: p.User.Username, CreatedAt: now, UpdatedAt: now, LastVerifiedAt: &now}
	if dp.CredentialBlob, err = dns.SealCredentials(s.box, dp.ID, creds); err != nil {
		s.writeErr(w, r, err)
		return
	}
	if err := s.cp.CreateDNSProvider(r.Context(), dp); err != nil {
		s.writeErr(w, r, err)
		return
	}
	s.audit.Record(r.Context(), p.actor(), "dns_provider.create", "dns_provider", dp.ID, "", map[string]any{"name": dp.Name, "type": dp.Type})
	writeJSON(w, 201, map[string]any{"provider": dp})
}

func (s *Server) handleUpdateDNSProvider(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	dp, err := s.store.GetDNSProvider(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	var in dnsProviderInput
	if err := decode(r, &in); err != nil {
		s.writeErr(w, r, err)
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" || len(in.Name) > 100 {
		s.writeErr(w, r, errValidation(map[string]string{"name": "Name is required (at most 100 characters)."}))
		return
	}
	if in.Type != "" && in.Type != dp.Type {
		s.writeErr(w, r, errValidation(map[string]string{"type": "The provider type cannot be changed."}))
		return
	}
	dp.Name = in.Name
	newCred := strings.TrimSpace(in.APIToken + in.APIKey)
	if newCred != "" {
		creds := dns.Credentials{APIToken: newCred}
		if dp.Type == "arvancloud" {
			creds = dns.Credentials{APIKey: newCred}
		}
		c, err := s.dns.NewClient(dp.Type, creds)
		if err == nil {
			err = s.verifyProvider(r.Context(), c)
		}
		if err != nil {
			s.dnsErr(w, r, err)
			return
		}
		if dp.CredentialBlob, err = dns.SealCredentials(s.box, dp.ID, creds); err != nil {
			s.writeErr(w, r, err)
			return
		}
		now := time.Now().UTC()
		dp.LastVerifiedAt, dp.LastError = &now, ""
	}
	dp.UpdatedAt = time.Now().UTC()
	if err := s.cp.UpdateDNSProvider(r.Context(), dp); err != nil {
		s.writeErr(w, r, err)
		return
	}
	s.audit.Record(r.Context(), p.actor(), "dns_provider.update", "dns_provider", dp.ID, "", map[string]any{"name": dp.Name, "credentials_changed": newCred != ""})
	writeJSON(w, 200, map[string]any{"provider": dp})
}

func (s *Server) handleDeleteDNSProvider(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	id := r.PathValue("id")
	dp, err := s.store.GetDNSProvider(r.Context(), id)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	wfs, err := s.store.ListWorkflows(r.Context())
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	for _, wf := range wfs {
		for _, st := range wf.Steps {
			if st.Type == "dns" && st.ProviderID == id {
				s.writeErr(w, r, &APIError{Status: 409, Code: "dns_provider_in_use", Message: "Workflow \"" + wf.Name + "\" uses this DNS integration. Remove the step first."})
				return
			}
		}
	}
	if plans, err := s.store.ListDRPlans(r.Context()); err == nil {
		if dp := dr.UsesProvider(plans, id); dp != nil {
			s.writeErr(w, r, &APIError{Status: 409, Code: "dns_provider_in_use", Message: "DR plan \"" + dp.Name + "\" uses this DNS integration."})
			return
		}
	}
	if err := s.cp.DeleteDNSProvider(r.Context(), id); err != nil {
		s.writeErr(w, r, err)
		return
	}
	s.audit.Record(r.Context(), p.actor(), "dns_provider.delete", "dns_provider", id, "", map[string]any{"name": dp.Name})
	w.WriteHeader(204)
}

func (s *Server) handleVerifyDNSProvider(w http.ResponseWriter, r *http.Request) {
	dp, err := s.store.GetDNSProvider(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	c, err := s.dns.Client(dp)
	if err == nil {
		err = s.verifyProvider(r.Context(), c)
	}
	now := time.Now().UTC()
	if err != nil {
		dp.LastError = dns.SafeError(err)
	} else {
		dp.LastError, dp.LastVerifiedAt = "", &now
	}
	if s.node.CanManage() {
		_ = s.cp.UpdateDNSProvider(r.Context(), dp)
	}
	writeJSON(w, 200, map[string]any{"ok": err == nil, "error": dp.LastError, "provider": dp})
}

func (s *Server) handleListDNSZones(w http.ResponseWriter, r *http.Request) {
	zs, err := s.dns.Zones(r.Context(), r.PathValue("id"), r.URL.Query().Get("fresh") == "1")
	if err != nil {
		s.dnsErr(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"zones": zs})
}

func (s *Server) handleListDNSRecords(w http.ResponseWriter, r *http.Request) {
	z, rs, err := s.dns.Records(r.Context(), r.PathValue("id"), r.PathValue("zone"))
	if err != nil {
		s.dnsErr(w, r, err)
		return
	}
	if rs == nil {
		rs = []dns.Record{}
	}
	writeJSON(w, 200, map[string]any{"zone": z, "records": rs, "editable_types": dns.EditableTypes})
}

func (s *Server) handlePlanDNSChange(w http.ResponseWriter, r *http.Request) {
	var req dns.ChangeRequest
	if err := decode(r, &req); err != nil {
		s.writeErr(w, r, err)
		return
	}
	p, err := s.dns.PlanChange(r.Context(), req)
	if err != nil {
		s.dnsErr(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"plan": p})
}

func (s *Server) handleApplyDNSChange(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	var in struct {
		Change dns.ChangeRequest `json:"change"`
		// Plan is the plan the user confirmed; the change is refused if the record changed since.
		Plan *dns.Plan `json:"plan"`
	}
	if err := decode(r, &in); err != nil {
		s.writeErr(w, r, err)
		return
	}
	if in.Plan == nil {
		s.writeErr(w, r, errValidation(map[string]string{"plan": "Review the plan (dry run) before applying a change."}))
		return
	}
	in.Change.Source = "manual"
	ch, err := s.dns.Apply(r.Context(), in.Change, in.Plan, p.User.Username)
	if ch == nil && err != nil {
		s.dnsErr(w, r, err)
		return
	}
	s.audit.Record(r.Context(), p.actor(), "dns_change.apply", "dns_change", ch.ID, "", map[string]any{"zone": ch.ZoneName, "action": ch.Action,
		"type": ch.RecordType, "name": ch.RecordName, "status": ch.Status, "snapshot_id": ch.SnapshotID})
	if err != nil {
		s.dnsErr(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"change": ch})
}

func (s *Server) handleListDNSChanges(w http.ResponseWriter, r *http.Request) {
	f := storage.DNSChangeFilter{ProviderID: r.URL.Query().Get("provider_id"), ZoneID: r.URL.Query().Get("zone_id"), Limit: queryInt(r, "limit", 100)}
	if st := r.URL.Query().Get("status"); st != "" {
		f.Statuses = strings.Split(st, ",")
	}
	chs, err := s.store.ListDNSChanges(r.Context(), f)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	if chs == nil {
		chs = []*model.DNSChange{}
	}
	writeJSON(w, 200, map[string]any{"changes": chs})
}

func (s *Server) handleGetDNSChange(w http.ResponseWriter, r *http.Request) {
	ch, err := s.store.GetDNSChange(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"change": ch})
}

func (s *Server) handleRevertDNSChange(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	var in struct {
		DryRun bool      `json:"dry_run"`
		Plan   *dns.Plan `json:"plan"`
	}
	if err := decode(r, &in); err != nil {
		s.writeErr(w, r, err)
		return
	}
	ch, err := s.store.GetDNSChange(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	req, err := dns.RevertRequest(ch)
	if err != nil {
		s.dnsErr(w, r, err)
		return
	}
	if in.DryRun {
		plan, err := s.dns.PlanChange(r.Context(), req)
		if err != nil {
			s.dnsErr(w, r, err)
			return
		}
		writeJSON(w, 200, map[string]any{"plan": plan})
		return
	}
	if in.Plan == nil {
		s.writeErr(w, r, errValidation(map[string]string{"plan": "Review the plan (dry run) before reverting."}))
		return
	}
	rv, err := s.dns.Apply(r.Context(), req, in.Plan, p.User.Username)
	if rv == nil && err != nil {
		s.dnsErr(w, r, err)
		return
	}
	s.audit.Record(r.Context(), p.actor(), "dns_change.revert", "dns_change", ch.ID, "", map[string]any{"revert_change_id": rv.ID, "status": rv.Status})
	if err != nil {
		s.dnsErr(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"change": rv})
}

func (s *Server) handleReconcileDNSChange(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	var in struct {
		Result string `json:"result"` // applied | not_applied
		Note   string `json:"note"`
	}
	if err := decode(r, &in); err != nil {
		s.writeErr(w, r, err)
		return
	}
	in.Note = strings.TrimSpace(in.Note)
	f := map[string]string{}
	if in.Result != "applied" && in.Result != "not_applied" {
		f["result"] = "Choose applied or not applied."
	}
	if in.Note == "" || len(in.Note) > 1000 {
		f["note"] = "Explain how you verified the outcome (required)."
	}
	if len(f) > 0 {
		s.writeErr(w, r, errValidation(f))
		return
	}
	id := r.PathValue("id")
	ch, err := s.dns.ReconcileManual(r.Context(), id, in.Result == "applied", p.User.Username, in.Note)
	if err != nil {
		s.dnsErr(w, r, err)
		return
	}
	// A change made by a workflow step shares the action's ID: settle the run too.
	if s.automation != nil {
		if a, err := s.store.GetAction(r.Context(), id); err == nil && a.Status == model.ActionUnknownOutcome {
			res := "failed"
			if in.Result == "applied" {
				res = "succeeded"
			}
			_, _ = s.automation.Reconcile(r.Context(), id, res, p.User.Username, in.Note)
		}
	}
	s.audit.Record(r.Context(), p.actor(), "dns_change.reconcile", "dns_change", id, "", map[string]any{"result": in.Result, "note": in.Note})
	writeJSON(w, 200, map[string]any{"change": ch})
}

func (s *Server) handleCreateDNSSnapshot(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	sn, err := s.dns.Snapshot(r.Context(), r.PathValue("id"), r.PathValue("zone"), "Manual snapshot", p.User.Username)
	if err != nil {
		s.dnsErr(w, r, err)
		return
	}
	s.audit.Record(r.Context(), p.actor(), "dns_snapshot.create", "dns_snapshot", sn.ID, "", map[string]any{"zone": sn.ZoneName, "records": sn.RecordCount})
	sn.Records = nil
	writeJSON(w, 201, map[string]any{"snapshot": sn})
}

func (s *Server) handleListDNSSnapshots(w http.ResponseWriter, r *http.Request) {
	sns, err := s.store.ListDNSSnapshots(r.Context(), r.URL.Query().Get("provider_id"), r.URL.Query().Get("zone_id"), queryInt(r, "limit", 100))
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	if sns == nil {
		sns = []*model.DNSSnapshot{}
	}
	writeJSON(w, 200, map[string]any{"snapshots": sns})
}

func (s *Server) handleGetDNSSnapshot(w http.ResponseWriter, r *http.Request) {
	sn, err := s.store.GetDNSSnapshot(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"snapshot": sn})
}

func (s *Server) handleRestoreDNSSnapshot(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	var in struct {
		DryRun         bool        `json:"dry_run"`
		IncludeDeletes bool        `json:"include_deletes"`
		Plans          []*dns.Plan `json:"plans"`
	}
	if err := decode(r, &in); err != nil {
		s.writeErr(w, r, err)
		return
	}
	sn, err := s.store.GetDNSSnapshot(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	reqs, plans, err := s.dns.RestorePlan(r.Context(), sn, in.IncludeDeletes)
	if err != nil {
		s.dnsErr(w, r, err)
		return
	}
	if plans == nil {
		plans = []*dns.Plan{}
	}
	if in.DryRun {
		writeJSON(w, 200, map[string]any{"plans": plans})
		return
	}
	// The confirmed plan must still be the current one.
	if len(in.Plans) != len(plans) {
		s.dnsErr(w, r, dns.ErrConflict)
		return
	}
	for i := range plans {
		if in.Plans[i] == nil || in.Plans[i].Action != plans[i].Action || !sameDNSRecord(in.Plans[i].Before, plans[i].Before) {
			s.dnsErr(w, r, dns.ErrConflict)
			return
		}
	}
	var out []*model.DNSChange
	var firstErr error
	for i, req := range reqs {
		ch, err := s.dns.Apply(r.Context(), req, plans[i], p.User.Username)
		if ch != nil {
			out = append(out, ch)
		}
		if err != nil && !errors.Is(err, dns.ErrNoChange) {
			firstErr = err
			break
		}
		if ch != nil && ch.Status != model.DNSChangeApplied && ch.Status != model.DNSChangeReconciledApplied {
			break // stop at the first change that did not definitely apply
		}
	}
	s.audit.Record(r.Context(), p.actor(), "dns_snapshot.restore", "dns_snapshot", sn.ID, "", map[string]any{"zone": sn.ZoneName, "planned": len(plans), "attempted": len(out), "include_deletes": in.IncludeDeletes})
	resp := map[string]any{"changes": out, "planned": len(plans)}
	if firstErr != nil {
		resp["error"] = dns.SafeError(firstErr)
	}
	writeJSON(w, 200, resp)
}

func sameDNSRecord(a, b *model.DNSRecord) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.ID == b.ID && a.Content == b.Content && a.TTL == b.TTL && a.Proxied == b.Proxied && strings.EqualFold(a.Name, b.Name) && a.Type == b.Type
}
