package api

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/intrabin/intrabin/internal/model"
)

type wfResp struct {
	Workflow map[string]any `json:"workflow"`
}

func TestWorkflowAPI(t *testing.T) {
	e := newEnv(t)
	owner := e.owner()
	var errResp struct {
		Error APIError `json:"error"`
	}
	bad := map[string]any{"name": "", "trigger": map[string]any{"type": "schedule", "interval_seconds": 5},
		"steps":   []map[string]any{{"type": "http", "url": "http://{{host}}/x"}, {"type": "script", "language": "ruby", "script": "x", "env": map[string]string{"LD_PRELOAD": "x"}}},
		"secrets": map[string]string{"PATH": "x"}}
	if code := owner.do("POST", "/api/v1/workflows", bad, &errResp); code != 422 {
		t.Fatalf("invalid workflow accepted: %d", code)
	}
	for _, k := range []string{"name", "trigger.interval_seconds", "steps.0.url", "steps.1.language", "steps.1.env", "secrets"} {
		if errResp.Error.Fields[k] == "" {
			t.Errorf("missing validation error for %s: %v", k, errResp.Error.Fields)
		}
	}

	in := map[string]any{"name": "Notify", "trigger": map[string]any{"type": "manual"},
		"steps":   []map[string]any{{"name": "pause", "type": "wait", "seconds": 1}, {"name": "hook", "type": "http", "url": "https://hooks.example.test/{{incident_id}}", "headers": map[string]string{"Authorization": "Bearer {{secret.TOKEN}}"}}},
		"secrets": map[string]string{"TOKEN": "very-secret-token"}}
	var created wfResp
	if code := owner.do("POST", "/api/v1/workflows", in, &created); code != 201 {
		t.Fatalf("create: %d", code)
	}
	id := created.Workflow["id"].(string)
	if created.Workflow["enabled"] != false {
		t.Fatal("workflows must be disabled by default")
	}
	var raw map[string]any
	owner.do("GET", "/api/v1/workflows/"+id, nil, &raw)
	body := mustJSON(raw)
	if strings.Contains(body, "very-secret-token") || !strings.Contains(body, `"TOKEN"`) {
		t.Fatalf("secret handling wrong: %s", body)
	}
	// Re-submitting a secret name with an empty value keeps the stored value.
	in["secrets"] = map[string]string{"TOKEN": ""}
	in["enabled"] = true
	var upd wfResp
	if code := owner.do("PUT", "/api/v1/workflows/"+id, in, &upd); code != 200 || upd.Workflow["version"].(float64) != 2 {
		t.Fatalf("update: %d %v", code, upd.Workflow["version"])
	}
	wf, _ := e.store.GetWorkflow(t.Context(), id)
	if len(wf.SecretBlob) == 0 || len(wf.SecretKeys) != 1 {
		t.Fatal("secret was dropped on update")
	}

	// Operators cannot run a workflow unless it allows them to.
	if code := owner.do("POST", "/api/v1/users", map[string]any{"username": "op1", "password": "operator-password", "role": "operator"}, nil); code != 201 {
		t.Fatalf("create operator %d", code)
	}
	op := e.client()
	op.do("POST", "/api/v1/auth/login", map[string]any{"username": "op1", "password": "operator-password"}, nil)
	op.loadCSRF()
	if code := op.do("POST", "/api/v1/workflows/"+id+"/run", nil, nil); code != 403 {
		t.Fatalf("operator ran a restricted workflow: %d", code)
	}
	if code := op.do("POST", "/api/v1/workflows", in, nil); code != 403 {
		t.Fatalf("operator created a workflow: %d", code)
	}

	var started struct{ Run model.WorkflowRun }
	if code := owner.do("POST", "/api/v1/workflows/"+id+"/run", map[string]any{"context": map[string]string{"note": "drill"}}, &started); code != 202 {
		t.Fatalf("run: %d", code)
	}
	// While the run is active the workflow cannot be deleted.
	if code := owner.do("DELETE", "/api/v1/workflows/"+id, nil, &errResp); code != 409 || errResp.Error.Code != "workflow_busy" {
		t.Fatalf("deleted a workflow with an active run: %d", code)
	}
	var cancelled struct{ Run model.WorkflowRun }
	if code := owner.do("POST", "/api/v1/workflow-runs/"+started.Run.ID+"/cancel", nil, &cancelled); code != 200 || cancelled.Run.Status != model.RunCancelled {
		t.Fatalf("cancel: %d %s", code, cancelled.Run.Status)
	}
	if code := owner.do("POST", "/api/v1/workflow-runs/"+started.Run.ID+"/cancel", nil, &errResp); code != 409 {
		t.Fatalf("cancel twice: %d", code)
	}
	time.Sleep(1500 * time.Millisecond) // the wait step would have ended; the run must stay cancelled
	var detail struct {
		Run     model.WorkflowRun
		Actions []model.WorkflowAction
	}
	owner.do("GET", "/api/v1/workflow-runs/"+started.Run.ID, nil, &detail)
	if detail.Run.Status != model.RunCancelled || len(detail.Actions) != 0 {
		t.Fatalf("cancelled run continued: %s, %d actions", detail.Run.Status, len(detail.Actions))
	}
	var list struct{ Runs []model.WorkflowRun }
	if code := op.do("GET", "/api/v1/workflow-runs?workflow_id="+id, nil, &list); code != 200 || len(list.Runs) != 1 || list.Runs[0].WorkflowName != "Notify" {
		t.Fatalf("list runs: %d %d", code, len(list.Runs))
	}
	var st map[string]any
	if code := owner.do("GET", "/api/v1/automation/status", nil, &st); code != 200 || st["executes_here"] != true {
		t.Fatalf("status: %d %v", code, st)
	}
	if code := owner.do("POST", "/api/v1/workflow-actions/00000000-0000-0000-0000-000000000000/reconcile", map[string]any{"result": "succeeded", "note": "x"}, nil); code != 404 {
		t.Fatalf("reconcile unknown action: %d", code)
	}
	if code := owner.do("DELETE", "/api/v1/workflows/"+id, nil, nil); code != 204 {
		t.Fatalf("delete: %d", code)
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
