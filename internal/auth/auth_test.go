package auth

import (
	"testing"
	"time"

	"github.com/intrabin/intrabin/internal/model"
)

func TestRBAC(t *testing.T) {
	if Can(model.RoleViewer, PermMonitorManage) {
		t.Fatal("viewer can manage monitors")
	}
	if !Can(model.RoleOperator, PermIncidentRespond) || Can(model.RoleOperator, PermUserManage) {
		t.Fatal("operator perms wrong")
	}
	if !Can(model.RoleAdmin, PermMonitorManage) {
		t.Fatal("admin cannot manage monitors")
	}
	if CanManageUser(model.RoleAdmin, model.RoleOwner, model.RoleOwner) {
		t.Fatal("admin can modify owner")
	}
	if CanManageUser(model.RoleAdmin, model.RoleViewer, model.RoleOwner) {
		t.Fatal("admin can promote to owner")
	}
	if !CanManageUser(model.RoleAdmin, model.RoleViewer, model.RoleOperator) {
		t.Fatal("admin cannot manage viewer")
	}
	if !CanManageUser(model.RoleOwner, model.RoleOwner, model.RoleAdmin) {
		t.Fatal("owner cannot manage owner")
	}
	if CanManageUser(model.RoleOperator, model.RoleViewer, model.RoleViewer) {
		t.Fatal("operator can manage users")
	}
}

func TestLoginLimiter(t *testing.T) {
	now := time.Unix(1000, 0)
	l := NewLoginLimiter()
	l.now = func() time.Time { return now }
	for i := 0; i < 4; i++ {
		l.Fail("ip:1")
	}
	if ok, _ := l.Allowed("ip:1"); !ok {
		t.Fatal("locked too early")
	}
	l.Fail("ip:1")
	ok, retry := l.Allowed("ip:1")
	if ok || retry != 15*time.Minute {
		t.Fatalf("expected lockout, got %v %v", ok, retry)
	}
	now = now.Add(16 * time.Minute)
	if ok, _ := l.Allowed("ip:1"); !ok {
		t.Fatal("lockout did not expire")
	}
	l.Fail("u:x")
	l.Succeed("u:x")
	if len(l.entries) != 1 {
		t.Fatal("succeed did not clear")
	}
}
