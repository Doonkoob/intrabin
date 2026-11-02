package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/intrabin/intrabin/internal/model"
)

const workflowCols = `id,name,description,enabled,trigger,steps,secret_env,secret_keys,operator_can_run,version,next_fire_at,created_by,created_at,updated_at,kind`

func (s *Store) CreateWorkflow(ctx context.Context, w *model.Workflow) error {
	tr, _ := json.Marshal(w.Trigger)
	st, _ := json.Marshal(nonNilSteps(w.Steps))
	_, err := s.DB.ExecContext(ctx, `INSERT INTO workflows(`+workflowCols+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		w.ID, w.Name, w.Description, boolInt(w.Enabled), string(tr), string(st), nullBlob(w.SecretBlob), secretKeysJSON(w.SecretKeys), boolInt(w.OperatorCanRun), w.Version, nullMs(w.NextFireAt), w.CreatedBy, ms(w.CreatedAt), ms(w.UpdatedAt), workflowKind(w.Kind))
	return mapConflict(err)
}

func workflowKind(k string) string {
	if k == "" {
		return "workflow"
	}
	return k
}

func (s *Store) UpdateWorkflow(ctx context.Context, w *model.Workflow) error {
	tr, _ := json.Marshal(w.Trigger)
	st, _ := json.Marshal(nonNilSteps(w.Steps))
	res, err := s.DB.ExecContext(ctx, `UPDATE workflows SET name=?,description=?,enabled=?,trigger=?,steps=?,secret_env=?,secret_keys=?,operator_can_run=?,version=?,next_fire_at=?,updated_at=? WHERE id=?`,
		w.Name, w.Description, boolInt(w.Enabled), string(tr), string(st), nullBlob(w.SecretBlob), secretKeysJSON(w.SecretKeys), boolInt(w.OperatorCanRun), w.Version, nullMs(w.NextFireAt), ms(w.UpdatedAt), w.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) DeleteWorkflow(ctx context.Context, id string) error {
	res, err := s.DB.ExecContext(ctx, `DELETE FROM workflows WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func scanWorkflow(sc interface{ Scan(...any) error }) (*model.Workflow, error) {
	var w model.Workflow
	var enabled, opRun int
	var tr, st, keys string
	var blob []byte
	var next sql.NullInt64
	var c, u int64
	if err := sc.Scan(&w.ID, &w.Name, &w.Description, &enabled, &tr, &st, &blob, &keys, &opRun, &w.Version, &next, &w.CreatedBy, &c, &u, &w.Kind); err != nil {
		return nil, err
	}
	w.Enabled, w.OperatorCanRun = enabled == 1, opRun == 1
	_ = json.Unmarshal([]byte(keys), &w.SecretKeys)
	if w.SecretKeys == nil {
		w.SecretKeys = []string{}
	}
	_ = json.Unmarshal([]byte(tr), &w.Trigger)
	_ = json.Unmarshal([]byte(st), &w.Steps)
	if w.Steps == nil {
		w.Steps = []model.WorkflowStep{}
	}
	if len(blob) > 0 {
		w.SecretBlob = blob
	}
	if next.Valid {
		t := fromMs(next.Int64)
		w.NextFireAt = &t
	}
	w.CreatedAt, w.UpdatedAt = fromMs(c), fromMs(u)
	return &w, nil
}

func (s *Store) GetWorkflow(ctx context.Context, id string) (*model.Workflow, error) {
	w, err := scanWorkflow(s.DB.QueryRowContext(ctx, `SELECT `+workflowCols+` FROM workflows WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return w, err
}

// ListWorkflows lists user-defined workflows (not the backing rows of DR plans).
func (s *Store) ListWorkflows(ctx context.Context) ([]*model.Workflow, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+workflowCols+` FROM workflows WHERE kind='workflow' ORDER BY name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*model.Workflow{}
	for rows.Next() {
		w, err := scanWorkflow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// ---- runs ----

const runCols = `id,workflow_id,workflow_version,definition,trigger_type,trigger_ref,context,status,current_step,next_at,created_by,created_at,started_at,finished_at,error,updated_at`

// CreateRun inserts a run. A duplicate (workflow_id, trigger_ref) returns ErrConflict, which makes
// incident-triggered runs idempotent.
func (s *Store) CreateRun(ctx context.Context, r *model.WorkflowRun) error {
	def, _ := json.Marshal(nonNilSteps(r.Steps))
	cx, _ := json.Marshal(r.Context)
	_, err := s.DB.ExecContext(ctx, `INSERT INTO workflow_runs(`+runCols+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.ID, r.WorkflowID, r.WorkflowVersion, string(def), r.TriggerType, r.TriggerRef, string(cx), r.Status, r.CurrentStep, nullMs(r.NextAt),
		r.CreatedBy, ms(r.CreatedAt), nullMs(r.StartedAt), nullMs(r.FinishedAt), r.Error, ms(r.UpdatedAt))
	return mapConflict(err)
}

// UpdateRun persists the mutable run fields.
func (s *Store) UpdateRun(ctx context.Context, r *model.WorkflowRun) error {
	res, err := s.DB.ExecContext(ctx, `UPDATE workflow_runs SET status=?,current_step=?,next_at=?,started_at=?,finished_at=?,error=?,updated_at=? WHERE id=?`,
		r.Status, r.CurrentStep, nullMs(r.NextAt), nullMs(r.StartedAt), nullMs(r.FinishedAt), r.Error, ms(r.UpdatedAt), r.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func scanRun(sc interface{ Scan(...any) error }) (*model.WorkflowRun, error) {
	var r model.WorkflowRun
	var def, cx string
	var next, started, finished sql.NullInt64
	var c, u int64
	if err := sc.Scan(&r.ID, &r.WorkflowID, &r.WorkflowVersion, &def, &r.TriggerType, &r.TriggerRef, &cx, &r.Status, &r.CurrentStep, &next, &r.CreatedBy, &c, &started, &finished, &r.Error, &u); err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(def), &r.Steps)
	_ = json.Unmarshal([]byte(cx), &r.Context)
	if r.Context == nil {
		r.Context = map[string]string{}
	}
	r.NextAt, r.StartedAt, r.FinishedAt = ptrNull(next), ptrNull(started), ptrNull(finished)
	r.CreatedAt, r.UpdatedAt = fromMs(c), fromMs(u)
	return &r, nil
}

func ptrNull(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := fromMs(v.Int64)
	return &t
}

func (s *Store) GetRun(ctx context.Context, id string) (*model.WorkflowRun, error) {
	r, err := scanRun(s.DB.QueryRowContext(ctx, `SELECT `+runCols+` FROM workflow_runs WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return r, err
}

// RunFilter selects runs.
type RunFilter struct {
	WorkflowID string
	Statuses   []string
	Limit      int
}

func (s *Store) ListRuns(ctx context.Context, f RunFilter) ([]*model.WorkflowRun, error) {
	q := `SELECT ` + prefixed("r.", runCols) + `, w.name FROM workflow_runs r JOIN workflows w ON w.id=r.workflow_id WHERE 1=1`
	var args []any
	if f.WorkflowID != "" {
		q += ` AND r.workflow_id=?`
		args = append(args, f.WorkflowID)
	}
	if len(f.Statuses) > 0 {
		q += ` AND r.status IN (?` + repeatComma(len(f.Statuses)-1) + `)`
		for _, st := range f.Statuses {
			args = append(args, st)
		}
	}
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 100
	}
	q += ` ORDER BY r.created_at DESC LIMIT ?`
	args = append(args, f.Limit)
	rows, err := s.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*model.WorkflowRun{}
	for rows.Next() {
		var name string
		r, err := scanRun(scanTail{rows, &name})
		if err != nil {
			return nil, err
		}
		r.WorkflowName = name
		out = append(out, r)
	}
	return out, rows.Err()
}

type scanTail struct {
	rows *sql.Rows
	tail *string
}

func (s scanTail) Scan(dest ...any) error { return s.rows.Scan(append(dest, s.tail)...) }

func repeatComma(n int) string {
	out := ""
	for i := 0; i < n; i++ {
		out += ",?"
	}
	return out
}

// ---- actions ----

const actionCols = `id,run_id,step_index,step_type,attempt,status,fence_term,fence_leader,started_at,finished_at,exit_code,output,error,reconciled_by`

// SaveAction upserts an action record.
func (s *Store) SaveAction(ctx context.Context, a *model.WorkflowAction) error {
	return saveActionTx(ctx, s.DB, a)
}

func scanAction(sc interface{ Scan(...any) error }) (*model.WorkflowAction, error) {
	var a model.WorkflowAction
	var term int64
	var started int64
	var finished, code sql.NullInt64
	if err := sc.Scan(&a.ID, &a.RunID, &a.StepIndex, &a.StepType, &a.Attempt, &a.Status, &term, &a.FenceLeader, &started, &finished, &code, &a.Output, &a.Error, &a.ReconciledBy); err != nil {
		return nil, err
	}
	a.FenceTerm = uint64(term)
	a.StartedAt = fromMs(started)
	a.FinishedAt = ptrNull(finished)
	if code.Valid {
		c := int(code.Int64)
		a.ExitCode = &c
	}
	return &a, nil
}

func (s *Store) GetAction(ctx context.Context, id string) (*model.WorkflowAction, error) {
	a, err := scanAction(s.DB.QueryRowContext(ctx, `SELECT `+actionCols+` FROM workflow_actions WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return a, err
}

func (s *Store) listActions(ctx context.Context, q string, args ...any) ([]*model.WorkflowAction, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+actionCols+` FROM workflow_actions `+q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*model.WorkflowAction{}
	for rows.Next() {
		a, err := scanAction(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// RunActions returns a run's actions in execution order.
func (s *Store) RunActions(ctx context.Context, runID string) ([]*model.WorkflowAction, error) {
	return s.listActions(ctx, `WHERE run_id=? ORDER BY step_index, attempt`, runID)
}

// DispatchingActions returns actions whose outcome was never recorded.
func (s *Store) DispatchingActions(ctx context.Context) ([]*model.WorkflowAction, error) {
	return s.listActions(ctx, `WHERE status=?`, model.ActionDispatching)
}

// PruneRuns deletes finished runs older than before.
func (s *Store) PruneRuns(ctx context.Context, before time.Time) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM workflow_runs WHERE finished_at IS NOT NULL AND finished_at < ?`, ms(before))
	return err
}

func nonNilSteps(s []model.WorkflowStep) []model.WorkflowStep {
	if s == nil {
		return []model.WorkflowStep{}
	}
	return s
}

func secretKeysJSON(k []string) string {
	if k == nil {
		k = []string{}
	}
	b, _ := json.Marshal(k)
	return string(b)
}

func nullBlob(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

func saveActionTx(ctx context.Context, x execer, a *model.WorkflowAction) error {
	var code any
	if a.ExitCode != nil {
		code = *a.ExitCode
	}
	_, err := x.ExecContext(ctx, `INSERT INTO workflow_actions(`+actionCols+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET status=excluded.status, finished_at=excluded.finished_at, exit_code=excluded.exit_code,
		output=excluded.output, error=excluded.error, reconciled_by=excluded.reconciled_by`,
		a.ID, a.RunID, a.StepIndex, a.StepType, a.Attempt, a.Status, int64(a.FenceTerm), a.FenceLeader, ms(a.StartedAt), nullMs(a.FinishedAt), code, a.Output, a.Error, a.ReconciledBy)
	return err
}

// ProgressRun atomically records an action (optional) and the run's new state.
func (s *Store) ProgressRun(ctx context.Context, r *model.WorkflowRun, a *model.WorkflowAction) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if a != nil {
		if err := saveActionTx(ctx, tx, a); err != nil {
			return err
		}
	}
	if r != nil {
		// A cancelled run stays cancelled: progress recorded by a step that was already executing
		// when the run was cancelled still stores its action, but does not revive the run.
		res, err := tx.ExecContext(ctx, `UPDATE workflow_runs SET status=?,current_step=?,next_at=?,started_at=?,finished_at=?,error=?,updated_at=?
			WHERE id=? AND (status<>'cancelled' OR ?='cancelled')`,
			r.Status, r.CurrentStep, nullMs(r.NextAt), nullMs(r.StartedAt), nullMs(r.FinishedAt), r.Error, ms(r.UpdatedAt), r.ID, r.Status)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			var one int
			if err := tx.QueryRowContext(ctx, `SELECT 1 FROM workflow_runs WHERE id=?`, r.ID).Scan(&one); err != nil {
				return ErrNotFound
			}
		}
	}
	return tx.Commit()
}

// FireSchedule atomically advances a scheduled workflow's next fire time and creates its run.
func (s *Store) FireSchedule(ctx context.Context, workflowID string, next time.Time, r *model.WorkflowRun) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE workflows SET next_fire_at=? WHERE id=?`, ms(next), workflowID); err != nil {
		return err
	}
	if r != nil {
		def, _ := json.Marshal(nonNilSteps(r.Steps))
		cx, _ := json.Marshal(r.Context)
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO workflow_runs(`+runCols+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			r.ID, r.WorkflowID, r.WorkflowVersion, string(def), r.TriggerType, r.TriggerRef, string(cx), r.Status, r.CurrentStep, nullMs(r.NextAt),
			r.CreatedBy, ms(r.CreatedAt), nullMs(r.StartedAt), nullMs(r.FinishedAt), r.Error, ms(r.UpdatedAt)); err != nil {
			return err
		}
	}
	return tx.Commit()
}
