package automation

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"

	"github.com/intrabin/intrabin/internal/model"
	"github.com/intrabin/intrabin/internal/security"
)

var placeholderRe = regexp.MustCompile(`\{\{\s*([a-zA-Z0-9_.]+)\s*\}\}`)

// expand replaces {{name}} with run context values ({{incident_id}}, {{monitor_name}}, …,
// {{run_id}}, {{workflow_id}}) and {{secret.NAME}} with workflow secrets. Unknown placeholders
// are left unchanged.
func expand(s string, run *model.WorkflowRun, secrets map[string]string) string {
	if !strings.Contains(s, "{{") {
		return s
	}
	return placeholderRe.ReplaceAllStringFunc(s, func(m string) string {
		key := placeholderRe.FindStringSubmatch(m)[1]
		if name, ok := strings.CutPrefix(key, "secret."); ok {
			if v, ok := secrets[name]; ok {
				return v
			}
			return m
		}
		switch key {
		case "run_id":
			return run.ID
		case "workflow_id":
			return run.WorkflowID
		}
		if v, ok := run.Context[key]; ok {
			return v
		}
		return m
	})
}

// redact removes secret values from text that is stored or displayed.
func redact(s string, secrets map[string]string) string {
	vals := make([]string, 0, len(secrets))
	for _, v := range secrets {
		if len(v) >= 4 {
			vals = append(vals, v)
		}
	}
	sort.Slice(vals, func(i, j int) bool { return len(vals[i]) > len(vals[j]) })
	for _, v := range vals {
		s = strings.ReplaceAll(s, v, "[REDACTED]")
	}
	return s
}

func secretAAD(workflowID string) []byte { return []byte("workflow:" + workflowID + ":secrets") }

// SealSecrets encrypts workflow secret environment variables with the cluster data key.
func SealSecrets(box *security.SecretBox, workflowID string, secrets map[string]string) ([]byte, error) {
	if len(secrets) == 0 {
		return nil, nil
	}
	b, _ := json.Marshal(secrets)
	return box.Seal(b, secretAAD(workflowID))
}

// OpenSecrets decrypts a workflow's secrets.
func OpenSecrets(box *security.SecretBox, wf *model.Workflow) (map[string]string, error) {
	out := map[string]string{}
	if len(wf.SecretBlob) == 0 {
		return out, nil
	}
	b, err := box.Open(wf.SecretBlob, secretAAD(wf.ID))
	if err != nil {
		return nil, err
	}
	return out, json.Unmarshal(b, &out)
}
