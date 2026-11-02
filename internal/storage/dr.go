package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/intrabin/intrabin/internal/model"
)

// ErrStateChanged is returned when a compare-and-set on a DR plan's state fails.
var ErrStateChanged = errors.New("the plan's state changed")

// SaveDRPlan creates or updates a DR plan together with its backing workflow row.
func (s *Store) SaveDRPlan(ctx context.Context, w *model.Workflow, p *model.DRPlan, create bool) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	w.Kind = "dr"
	tr, _ := json.Marshal(w.Trigger)
	st, _ := json.Marshal(nonNilSteps(w.Steps))
	cfg, _ := json.Marshal(p.Config)
	if create {
		if _, err := tx.ExecContext(ctx, `INSERT INTO workflows(`+workflowCols+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			w.ID, w.Name, w.Description, 0, string(tr), string(st), nullBlob(w.SecretBlob), secretKeysJSON(w.SecretKeys), 0, w.Version, nil, w.CreatedBy, ms(w.CreatedAt), ms(w.UpdatedAt), "dr"); err != nil {
			return mapConflict(err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO dr_plans(id,config,state,active_run_id,direction,state_note,last_failover_at,last_transition_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?)`,
			p.ID, string(cfg), p.State, "", "", "", nil, ms(p.LastTransitionAt), ms(p.UpdatedAt)); err != nil {
			return mapConflict(err)
		}
	} else {
		res, err := tx.ExecContext(ctx, `UPDATE workflows SET name=?,description=?,secret_env=?,secret_keys=?,version=?,updated_at=? WHERE id=? AND kind='dr'`,
			w.Name, w.Description, nullBlob(w.SecretBlob), secretKeysJSON(w.SecretKeys), w.Version, ms(w.UpdatedAt), w.ID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		if _, err := tx.ExecContext(ctx, `UPDATE dr_plans SET config=?,updated_at=? WHERE id=?`, string(cfg), ms(p.UpdatedAt), p.ID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

const drCols = `w.id,w.name,w.description,w.secret_keys,w.version,w.created_by,w.created_at,d.config,d.state,d.active_run_id,d.direction,d.state_note,d.last_failover_at,d.last_transition_at,d.updated_at`

func scanDRPlan(sc interface{ Scan(...any) error }) (*model.DRPlan, error) {
	var p model.DRPlan
	var keys, cfg string
	var lf sql.NullInt64
	var c, lt, u int64
	if err := sc.Scan(&p.ID, &p.Name, &p.Description, &keys, &p.Version, &p.CreatedBy, &c, &cfg, &p.State, &p.ActiveRunID, &p.Direction, &p.StateNote, &lf, &lt, &u); err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(keys), &p.SecretKeys)
	if p.SecretKeys == nil {
		p.SecretKeys = []string{}
	}
	_ = json.Unmarshal([]byte(cfg), &p.Config)
	if p.Config.MonitorIDs == nil {
		p.Config.MonitorIDs = []string{}
	}
	p.Config.PreSteps, p.Config.PostSteps = nonNilSteps(p.Config.PreSteps), nonNilSteps(p.Config.PostSteps)
	p.LastFailoverAt = ptrNull(lf)
	p.CreatedAt, p.LastTransitionAt, p.UpdatedAt = fromMs(c), fromMs(lt), fromMs(u)
	return &p, nil
}

func (s *Store) GetDRPlan(ctx context.Context, id string) (*model.DRPlan, error) {
	p, err := scanDRPlan(s.DB.QueryRowContext(ctx, `SELECT `+drCols+` FROM dr_plans d JOIN workflows w ON w.id=d.id WHERE d.id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return p, err
}

func (s *Store) ListDRPlans(ctx context.Context) ([]*model.DRPlan, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+drCols+` FROM dr_plans d JOIN workflows w ON w.id=d.id ORDER BY w.name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*model.DRPlan{}
	for rows.Next() {
		p, err := scanDRPlan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// DRTransition is a compare-and-set on a plan's state, optionally creating the run that drives it.
type DRTransition struct {
	PlanID string   `json:"plan_id"`
	Expect []string `json:"expect"` // allowed current states
	// ExpectRunID, when set, must equal the plan's active run (guards run-completion updates).
	ExpectRunID string             `json:"expect_run_id,omitempty"`
	State       string             `json:"state"`
	Direction   string             `json:"direction,omitempty"`
	Note        string             `json:"note"`
	Run         *model.WorkflowRun `json:"run,omitempty"`
	At          time.Time          `json:"at"`
}

// TransitionDRPlan applies t atomically. ErrStateChanged if the plan is not in an expected state.
func (s *Store) TransitionDRPlan(ctx context.Context, t DRTransition) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var cur, runID string
	if err := tx.QueryRowContext(ctx, `SELECT state, active_run_id FROM dr_plans WHERE id=?`, t.PlanID).Scan(&cur, &runID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if !slices.Contains(t.Expect, cur) || (t.ExpectRunID != "" && t.ExpectRunID != runID) {
		return ErrStateChanged
	}
	q := `UPDATE dr_plans SET state=?,state_note=?,last_transition_at=?,updated_at=?`
	args := []any{t.State, t.Note, ms(t.At), ms(t.At)}
	if t.Run != nil {
		def, _ := json.Marshal(nonNilSteps(t.Run.Steps))
		cx, _ := json.Marshal(t.Run.Context)
		r := t.Run
		if _, err := tx.ExecContext(ctx, `INSERT INTO workflow_runs(`+runCols+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			r.ID, r.WorkflowID, r.WorkflowVersion, string(def), r.TriggerType, r.TriggerRef, string(cx), r.Status, r.CurrentStep, nullMs(r.NextAt),
			r.CreatedBy, ms(r.CreatedAt), nullMs(r.StartedAt), nullMs(r.FinishedAt), r.Error, ms(r.UpdatedAt)); err != nil {
			return mapConflict(err)
		}
		q += `,active_run_id=?,direction=?`
		args = append(args, r.ID, t.Direction)
		if t.Direction == "failover" {
			q += `,last_failover_at=?`
			args = append(args, ms(t.At))
		}
	}
	q += ` WHERE id=?`
	args = append(args, t.PlanID)
	if _, err := tx.ExecContext(ctx, q, args...); err != nil {
		return err
	}
	return tx.Commit()
}
