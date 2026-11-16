package node

import (
	"context"
	"strings"
	"testing"

	"github.com/intrabin/intrabin/internal/storage"
)

func TestIdentitySurvivesRestartAndNameChange(t *testing.T) {
	ctx := context.Background()
	st, err := storage.Open(ctx, t.TempDir()+"/db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	first, err := LoadIdentity(ctx, st, "")
	if err != nil {
		t.Fatal(err)
	}
	saved, err := st.GetSetting(ctx, "node.name")
	if err != nil || saved != first.Identity.Name {
		t.Fatalf("name not persisted: %q %v", saved, err)
	}
	next, err := LoadIdentity(ctx, st, "new-hostname")
	if err != nil || next.Identity.ID != first.Identity.ID {
		t.Fatalf("identity changed: %+v %v", next, err)
	}
	restored, err := LoadIdentity(ctx, st, "")
	if err != nil || restored.Identity.Name != "new-hostname" {
		t.Fatalf("name not restored: %+v %v", restored, err)
	}
}

func TestLabelsDistinguishDuplicateHostnames(t *testing.T) {
	a := Label("11111111-1111", "same-host", "https://node-one.example")
	b := Label("22222222-2222", "same-host", "203.0.113.2:7443")
	if a == b || !strings.Contains(a, "node-one.example") || !strings.Contains(b, "203.0.113.2:7443") {
		t.Fatalf("ambiguous labels %q %q", a, b)
	}
	if Label("11111111-1111", "same-host", "") == Label("22222222-2222", "same-host", "") {
		t.Fatal("fallback labels must include stable identity")
	}
}
