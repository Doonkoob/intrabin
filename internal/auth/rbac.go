// Package auth implements sessions, role-based access control and login throttling.
package auth

import "github.com/intrabin/intrabin/internal/model"

// Permission is a capability checked by API handlers.
type Permission string

const (
	PermView             Permission = "view"              // dashboards, monitors, incidents
	PermIncidentRespond  Permission = "incident.respond"  // acknowledge/resolve/notes
	PermMonitorManage    Permission = "monitor.manage"    // create/edit/delete monitors, groups, maintenance
	PermUserManage       Permission = "user.manage"       // manage users (not owners unless owner)
	PermSettingsManage   Permission = "settings.manage"   // global settings
	PermAuditView        Permission = "audit.view"        // audit logs
	PermClusterManage    Permission = "cluster.manage"    // join/remove/promote nodes (Phase 2)
	PermAutomationManage Permission = "automation.manage" // Phase 3
	PermAutomationRun    Permission = "automation.run"    // Phase 3 (operators: explicitly permitted workflows)
	PermDNSManage        Permission = "dns.manage"        // Phase 4: DNS integrations and record changes
	PermDRManage         Permission = "dr.manage"         // Phase 5: DR plans, failover and failback
)

var rolePerms = map[model.Role][]Permission{
	model.RoleViewer:   {PermView},
	model.RoleOperator: {PermView, PermIncidentRespond, PermAutomationRun},
	model.RoleAdmin: {PermView, PermIncidentRespond, PermAutomationRun, PermMonitorManage, PermUserManage,
		PermSettingsManage, PermAuditView, PermClusterManage, PermAutomationManage, PermDNSManage, PermDRManage},
	model.RoleOwner: {PermView, PermIncidentRespond, PermAutomationRun, PermMonitorManage, PermUserManage,
		PermSettingsManage, PermAuditView, PermClusterManage, PermAutomationManage, PermDNSManage, PermDRManage},
}

// Can reports whether role has permission p.
func Can(role model.Role, p Permission) bool {
	for _, x := range rolePerms[role] {
		if x == p {
			return true
		}
	}
	return false
}

// Permissions lists the permissions of a role.
func Permissions(role model.Role) []Permission {
	return append([]Permission(nil), rolePerms[role]...)
}

// CanManageUser reports whether actor may modify target (and assign newRole).
// Admins may manage operators/viewers and other admins but never owners;
// only owners may create or modify owners.
func CanManageUser(actor model.Role, target model.Role, newRole model.Role) bool {
	if !Can(actor, PermUserManage) {
		return false
	}
	if actor == model.RoleOwner {
		return true
	}
	return target != model.RoleOwner && newRole != model.RoleOwner
}
