package controlplane

import (
	"context"
	"time"

	"github.com/intrabin/intrabin/internal/model"
	"github.com/intrabin/intrabin/internal/storage"
)

// SetSetting writes a setting. Node-local keys (node.*, local.*) bypass replication.
func (c *CP) SetSetting(ctx context.Context, key, value string) error {
	if storage.IsLocalSetting(key) {
		return c.st.SetSetting(ctx, key, value)
	}
	_, err := c.apply(ctx, "setting.set", settingPayload{Key: key, Value: value})
	return err
}

func (c *CP) CreateUser(ctx context.Context, u *model.User) error {
	_, err := c.apply(ctx, "user.create", userWire{User: *u, PasswordHash: u.PasswordHash})
	return err
}

func (c *CP) CreateFirstUser(ctx context.Context, u *model.User) error {
	_, err := c.apply(ctx, "user.create_first", userWire{User: *u, PasswordHash: u.PasswordHash})
	return err
}

func (c *CP) UpdateUser(ctx context.Context, u *model.User) error {
	at, err := c.apply(ctx, "user.update", userWire{User: *u})
	if err == nil {
		u.UpdatedAt = at
	}
	return err
}

func (c *CP) SetPassword(ctx context.Context, userID, hash string) error {
	_, err := c.apply(ctx, "user.set_password", passwordPayload{UserID: userID, Hash: hash})
	return err
}

func (c *CP) TouchLogin(ctx context.Context, userID string) error {
	_, err := c.apply(ctx, "user.touch_login", idPayload{ID: userID})
	return err
}

func (c *CP) DeleteUser(ctx context.Context, id string) error {
	_, err := c.apply(ctx, "user.delete", idPayload{ID: id})
	return err
}

func (c *CP) CreateSession(ctx context.Context, se *model.Session) error {
	_, err := c.apply(ctx, "session.create", se)
	return err
}

func (c *CP) TouchSession(ctx context.Context, hash string, at time.Time) error {
	_, err := c.apply(ctx, "session.touch", touchPayload{Hash: hash, At: at})
	return err
}

func (c *CP) DeleteSession(ctx context.Context, hash string) error {
	_, err := c.apply(ctx, "session.delete", idPayload{ID: hash})
	return err
}

func (c *CP) DeleteUserSessions(ctx context.Context, userID string) error {
	_, err := c.apply(ctx, "session.delete_user", idPayload{ID: userID})
	return err
}

func (c *CP) DeleteExpiredSessions(ctx context.Context, now time.Time, idle time.Duration) error {
	_, err := c.apply(ctx, "session.delete_expired", expirePayload{Now: now, Idle: idle})
	return err
}

func (c *CP) CreateMonitor(ctx context.Context, m *model.Monitor) error {
	_, err := c.apply(ctx, "monitor.create", monitorWire{Monitor: *m, SecretBlob: m.SecretBlob})
	return err
}

func (c *CP) UpdateMonitor(ctx context.Context, m *model.Monitor) error {
	_, err := c.apply(ctx, "monitor.update", monitorWire{Monitor: *m, SecretBlob: m.SecretBlob})
	return err
}

func (c *CP) DeleteMonitor(ctx context.Context, id string) error {
	_, err := c.apply(ctx, "monitor.delete", idPayload{ID: id})
	return err
}

func (c *CP) SaveMonitorState(ctx context.Context, s *model.MonitorState) error {
	_, err := c.apply(ctx, "monitor.state", s)
	return err
}

func (c *CP) RecordRound(ctx context.Context, rr *model.RoundResult, s *model.MonitorState, interval int) error {
	_, err := c.apply(ctx, "monitor.round", roundPayload{Round: rr, State: s, Interval: interval})
	return err
}

func (c *CP) CreateGroup(ctx context.Context, g *model.MonitorGroup) error {
	_, err := c.apply(ctx, "group.create", g)
	return err
}

func (c *CP) UpdateGroup(ctx context.Context, g *model.MonitorGroup) error {
	_, err := c.apply(ctx, "group.update", g)
	return err
}

func (c *CP) DeleteGroup(ctx context.Context, id string) error {
	_, err := c.apply(ctx, "group.delete", idPayload{ID: id})
	return err
}

func (c *CP) CreateMaintenance(ctx context.Context, w *model.MaintenanceWindow) error {
	_, err := c.apply(ctx, "maintenance.create", w)
	return err
}

func (c *CP) DeleteMaintenance(ctx context.Context, id string) error {
	_, err := c.apply(ctx, "maintenance.delete", idPayload{ID: id})
	return err
}

func (c *CP) SaveIncident(ctx context.Context, in *model.Incident, events ...*model.IncidentEvent) error {
	_, err := c.apply(ctx, "incident.save", incidentPayload{Incident: in, Events: events})
	return err
}

func (c *CP) InsertAudit(ctx context.Context, a *model.AuditEntry) error {
	_, err := c.apply(ctx, "audit.insert", a)
	return err
}

func (c *CP) CreateEnrollmentToken(ctx context.Context, t *model.EnrollmentToken) error {
	_, err := c.apply(ctx, "token.create", tokenPayload{Token: t, SecretHash: t.SecretHash})
	return err
}

func (c *CP) ConsumeEnrollmentToken(ctx context.Context, id, secretHash, nodeID string) error {
	_, err := c.apply(ctx, "token.consume", consumePayload{ID: id, SecretHash: secretHash, NodeID: nodeID})
	return err
}

func (c *CP) RevokeEnrollmentToken(ctx context.Context, id string) error {
	_, err := c.apply(ctx, "token.revoke", idPayload{ID: id})
	return err
}

func (c *CP) UpsertClusterNode(ctx context.Context, n *model.ClusterNode) error {
	_, err := c.apply(ctx, "node.upsert", n)
	return err
}

func (c *CP) DeleteClusterNode(ctx context.Context, id string) error {
	_, err := c.apply(ctx, "node.delete", idPayload{ID: id})
	return err
}

// RestoreState replaces all replicated state with a dump (used to seed a new cluster).
func (c *CP) RestoreState(ctx context.Context, dump []byte) error {
	_, err := c.apply(ctx, "state.restore", restorePayload{Dump: dump})
	return err
}

// IncidentStore reads incidents from the local store and writes them through the control plane.
type IncidentStore struct {
	*storage.Store
	CP *CP
}

func (s IncidentStore) SaveIncident(ctx context.Context, in *model.Incident, events ...*model.IncidentEvent) error {
	return s.CP.SaveIncident(ctx, in, events...)
}

func (c *CP) CreateWorkflow(ctx context.Context, w *model.Workflow) error {
	_, err := c.apply(ctx, "workflow.create", workflowWire{Workflow: *w, SecretBlob: w.SecretBlob})
	return err
}

func (c *CP) UpdateWorkflow(ctx context.Context, w *model.Workflow) error {
	_, err := c.apply(ctx, "workflow.update", workflowWire{Workflow: *w, SecretBlob: w.SecretBlob})
	return err
}

func (c *CP) DeleteWorkflow(ctx context.Context, id string) error {
	_, err := c.apply(ctx, "workflow.delete", idPayload{ID: id})
	return err
}

// FireSchedule advances a schedule and creates its run in one replicated step.
func (c *CP) FireSchedule(ctx context.Context, workflowID string, next time.Time, r *model.WorkflowRun) error {
	_, err := c.apply(ctx, "workflow.fire", firePayload{WorkflowID: workflowID, Next: next, Run: r})
	return err
}

func (c *CP) CreateRun(ctx context.Context, r *model.WorkflowRun) error {
	_, err := c.apply(ctx, "run.create", r)
	return err
}

// ProgressRun records an action and/or the run's new state atomically.
func (c *CP) ProgressRun(ctx context.Context, r *model.WorkflowRun, a *model.WorkflowAction) error {
	_, err := c.apply(ctx, "run.progress", progressPayload{Run: r, Action: a})
	return err
}

func (c *CP) PruneRuns(ctx context.Context, before time.Time) error {
	_, err := c.apply(ctx, "runs.prune", expirePayload{Now: before})
	return err
}

func (c *CP) CreateDNSProvider(ctx context.Context, p *model.DNSProvider) error {
	_, err := c.apply(ctx, "dnsprovider.create", dnsProviderWire{DNSProvider: *p, CredentialBlob: p.CredentialBlob})
	return err
}

func (c *CP) UpdateDNSProvider(ctx context.Context, p *model.DNSProvider) error {
	_, err := c.apply(ctx, "dnsprovider.update", dnsProviderWire{DNSProvider: *p, CredentialBlob: p.CredentialBlob})
	return err
}

func (c *CP) DeleteDNSProvider(ctx context.Context, id string) error {
	_, err := c.apply(ctx, "dnsprovider.delete", idPayload{ID: id})
	return err
}

func (c *CP) SaveDNSSnapshot(ctx context.Context, s *model.DNSSnapshot) error {
	_, err := c.apply(ctx, "dnssnapshot.save", s)
	return err
}

func (c *CP) SaveDNSChange(ctx context.Context, ch *model.DNSChange) error {
	_, err := c.apply(ctx, "dnschange.save", ch)
	return err
}

func (c *CP) PruneDNSSnapshots(ctx context.Context, before time.Time, keep int) error {
	_, err := c.apply(ctx, "dnssnapshots.prune", dnsPrunePayload{Before: before, Keep: keep})
	return err
}

type drPlanWire struct {
	Workflow   model.Workflow `json:"workflow"`
	SecretBlob []byte         `json:"secret_blob"`
	Plan       model.DRPlan   `json:"plan"`
}

// SaveDRPlan creates or updates a DR plan with its backing workflow row (secrets live there).
func (c *CP) SaveDRPlan(ctx context.Context, w *model.Workflow, p *model.DRPlan, create bool) error {
	op := "drplan.update"
	if create {
		op = "drplan.create"
	}
	_, err := c.apply(ctx, op, drPlanWire{Workflow: *w, SecretBlob: w.SecretBlob, Plan: *p})
	return err
}

// TransitionDRPlan is an atomic compare-and-set of a plan's state (and optionally starts a run).
func (c *CP) TransitionDRPlan(ctx context.Context, t storage.DRTransition) error {
	_, err := c.apply(ctx, "drplan.transition", t)
	return err
}
