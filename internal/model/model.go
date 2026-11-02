// Package model holds the domain types shared between storage, the monitoring
// engine, the incident manager and the API.
package model

import "time"

// Role is a user role. Roles are ordered: owner > admin > operator > viewer.
type Role string

const (
	RoleOwner    Role = "owner"
	RoleAdmin    Role = "admin"
	RoleOperator Role = "operator"
	RoleViewer   Role = "viewer"
)

// Rank returns a comparable rank for a role (higher = more privileged).
func (r Role) Rank() int {
	switch r {
	case RoleOwner:
		return 4
	case RoleAdmin:
		return 3
	case RoleOperator:
		return 2
	case RoleViewer:
		return 1
	}
	return 0
}

// Valid reports whether r is a known role.
func (r Role) Valid() bool { return r.Rank() > 0 }

type User struct {
	ID                string     `json:"id"`
	Username          string     `json:"username"`
	DisplayName       string     `json:"display_name"`
	PasswordHash      string     `json:"-"`
	Role              Role       `json:"role"`
	Disabled          bool       `json:"disabled"`
	Language          string     `json:"language"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
	PasswordChangedAt time.Time  `json:"password_changed_at"`
	LastLoginAt       *time.Time `json:"last_login_at,omitempty"`
}

type Session struct {
	TokenHash  string
	UserID     string
	CSRFToken  string
	CreatedAt  time.Time
	LastSeenAt time.Time
	ExpiresAt  time.Time
	IP         string
	UserAgent  string
}

// MonitorType enumerates supported check types.
type MonitorType string

const (
	MonitorHTTP MonitorType = "http"
	MonitorTCP  MonitorType = "tcp"
	MonitorDNS  MonitorType = "dns"
	MonitorTLS  MonitorType = "tls"
)

func (t MonitorType) Valid() bool {
	switch t {
	case MonitorHTTP, MonitorTCP, MonitorDNS, MonitorTLS:
		return true
	}
	return false
}

// MonitorSettings holds type-specific, non-secret configuration.
type MonitorSettings struct {
	// HTTP
	Method          string            `json:"method,omitempty"`
	Headers         map[string]string `json:"headers,omitempty"`
	Body            string            `json:"body,omitempty"`
	ExpectedStatus  []string          `json:"expected_status,omitempty"` // e.g. "200", "2xx", "200-299"
	ExpectedBody    string            `json:"expected_body,omitempty"`   // substring that must be present
	FollowRedirects *bool             `json:"follow_redirects,omitempty"`
	MaxRedirects    int               `json:"max_redirects,omitempty"`
	// TLS verification. Defaults to verifying (safe default).
	SkipTLSVerify bool `json:"skip_tls_verify,omitempty"`
	// Origin checks: connect to OriginAddress (ip or ip:port) instead of resolving
	// the URL host, while keeping Host header and TLS SNI from the URL (or overrides).
	OriginAddress string `json:"origin_address,omitempty"`
	HostHeader    string `json:"host_header,omitempty"`
	TLSServerName string `json:"tls_server_name,omitempty"`
	// Mark DEGRADED if latency exceeds this (0 disables).
	DegradedLatencyMs int `json:"degraded_latency_ms,omitempty"`
	// TLS / HTTPS certificate expiry thresholds (days).
	CertExpiryWarnDays int `json:"cert_expiry_warn_days,omitempty"`
	CertExpiryFailDays int `json:"cert_expiry_fail_days,omitempty"`
	// DNS
	RecordType    string `json:"record_type,omitempty"`
	ExpectedValue string `json:"expected_value,omitempty"`
	Resolver      string `json:"resolver,omitempty"` // ip:port, optional
	// Path label for diagnostics: public, origin, internal
	CheckPath string `json:"check_path,omitempty"`
}

// MonitorSecrets are encrypted at rest and never returned by the API.
type MonitorSecrets struct {
	BasicAuthUser     string            `json:"basic_auth_user,omitempty"`
	BasicAuthPassword string            `json:"basic_auth_password,omitempty"`
	SecretHeaders     map[string]string `json:"secret_headers,omitempty"`
}

// FailMode selects how failed rounds escalate to DOWN.
type FailMode string

const (
	FailConsecutive FailMode = "consecutive"
	FailWindow      FailMode = "window"
)

// Policy defines multi-location verification semantics.
type Policy struct {
	// Number of failing probes (distinct nodes) required to mark a round failed.
	ConfirmationsRequired int `json:"confirmations_required"`
	// Minimum number of distinct monitoring locations among failing nodes.
	MinLocations int `json:"min_locations"`
	// Minimum valid (fresh) probes for a round to be conclusive.
	MinValidProbes int      `json:"min_valid_probes"`
	FailMode       FailMode `json:"fail_mode"`
	// consecutive: failed rounds in a row; window: failed rounds within WindowSeconds.
	FailureRounds int `json:"failure_rounds"`
	WindowSeconds int `json:"window_seconds,omitempty"`
	// Consecutive ok rounds required to recover.
	RecoveryRounds int `json:"recovery_rounds"`
	// Failure within this many seconds after resolution reopens the incident.
	CooldownSeconds int `json:"cooldown_seconds"`
}

// DefaultPolicy is safe for a standalone node.
func DefaultPolicy() Policy {
	return Policy{
		ConfirmationsRequired: 1,
		MinLocations:          1,
		MinValidProbes:        1,
		FailMode:              FailConsecutive,
		FailureRounds:         3,
		RecoveryRounds:        2,
		CooldownSeconds:       300,
	}
}

type Monitor struct {
	ID              string          `json:"id"`
	Name            string          `json:"name"`
	Description     string          `json:"description"`
	Type            MonitorType     `json:"type"`
	Target          string          `json:"target"`
	GroupID         string          `json:"group_id,omitempty"`
	Tags            []string        `json:"tags"`
	IntervalSeconds int             `json:"interval_seconds"`
	TimeoutSeconds  int             `json:"timeout_seconds"`
	Retries         int             `json:"retries"`
	Enabled         bool            `json:"enabled"`
	Settings        MonitorSettings `json:"settings"`
	Secrets         *MonitorSecrets `json:"-"`           // decrypted, populated only for probe execution
	SecretBlob      []byte          `json:"-"`           // encrypted MonitorSecrets as stored
	SecretKeys      []string        `json:"secret_keys"` // names of configured secrets, values never exposed
	Policy          Policy          `json:"policy"`
	NodeSelector    []string        `json:"node_ids"`
	Version         int             `json:"version"`
	CreatedAt       time.Time       `json:"created_at"`
	UpdatedAt       time.Time       `json:"updated_at"`
}

type MonitorGroup struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// MonitorStatus is the evaluated availability state of a monitor.
type MonitorStatus string

const (
	StatusUp            MonitorStatus = "UP"
	StatusDegraded      MonitorStatus = "DEGRADED"
	StatusSuspectedDown MonitorStatus = "SUSPECTED_DOWN"
	StatusDown          MonitorStatus = "DOWN"
	StatusUnknown       MonitorStatus = "UNKNOWN"
	StatusPaused        MonitorStatus = "PAUSED"
	StatusMaintenance   MonitorStatus = "MAINTENANCE"
)

type MonitorState struct {
	MonitorID               string        `json:"monitor_id"`
	Status                  MonitorStatus `json:"status"`
	ConsecutiveFailed       int           `json:"consecutive_failed"`
	ConsecutiveOK           int           `json:"consecutive_ok"`
	ConsecutiveInconclusive int           `json:"consecutive_inconclusive"`
	LastRound               int64         `json:"last_round"` // unix ms of last evaluated round start
	LastVerdict             Verdict       `json:"last_verdict"`
	LastLatencyMs           *float64      `json:"last_latency_ms,omitempty"`
	StatusSince             time.Time     `json:"status_since"`
	UpdatedAt               time.Time     `json:"updated_at"`
}

// ErrorCategory classifies failures precisely. A timeout is never an HTTP 5xx.
type ErrorCategory string

const (
	ErrNone               ErrorCategory = ""
	ErrHTTP5xx            ErrorCategory = "http_5xx"
	ErrUnexpectedStatus   ErrorCategory = "unexpected_status"
	ErrBodyMismatch       ErrorCategory = "body_mismatch"
	ErrTimeout            ErrorCategory = "timeout"
	ErrDNS                ErrorCategory = "dns_failure"
	ErrConnectionRefused  ErrorCategory = "connection_refused"
	ErrNetworkUnreachable ErrorCategory = "network_unreachable"
	ErrConnectionReset    ErrorCategory = "connection_reset"
	ErrTLS                ErrorCategory = "tls_error"
	ErrCertExpiring       ErrorCategory = "cert_expiring"
	ErrDNSMismatch        ErrorCategory = "dns_mismatch"
	ErrInvalidConfig      ErrorCategory = "invalid_config"
	ErrUnknown            ErrorCategory = "unknown"
)

type CheckResult struct {
	ID            string        `json:"id"`
	MonitorID     string        `json:"monitor_id"`
	NodeID        string        `json:"node_id"`
	ConfigVersion int           `json:"config_version"`
	RoundStart    time.Time     `json:"round_start"`
	ProbeAt       time.Time     `json:"probe_at"`
	ReceivedAt    time.Time     `json:"received_at"`
	Success       bool          `json:"success"`
	Slow          bool          `json:"slow"`
	LatencyMs     *float64      `json:"latency_ms,omitempty"`
	HTTPStatus    *int          `json:"http_status,omitempty"`
	ErrorCategory ErrorCategory `json:"error_category"`
	ErrorMessage  string        `json:"error_message"`
	Attempts      int           `json:"attempts"`
	CertExpiresAt *time.Time    `json:"cert_expires_at,omitempty"`
}

// Verdict is the evaluated outcome of a monitoring round.
type Verdict string

const (
	VerdictOK           Verdict = "ok"
	VerdictPartial      Verdict = "partial"
	VerdictFailed       Verdict = "failed"
	VerdictInconclusive Verdict = "inconclusive"
	VerdictMaintenance  Verdict = "maintenance"
)

type RoundResult struct {
	MonitorID     string    `json:"monitor_id"`
	RoundStart    time.Time `json:"round_start"`
	Verdict       Verdict   `json:"verdict"`
	ValidProbes   int       `json:"valid_probes"`
	FailingProbes int       `json:"failing_probes"`
	FailingNodes  []string  `json:"failing_nodes"`
	AvgLatencyMs  *float64  `json:"avg_latency_ms,omitempty"`
	Slow          bool      `json:"slow"`
	EvaluatedAt   time.Time `json:"evaluated_at"`
	// NodeStats are the per-node probe results of the round (replicated with the round so every
	// node stores the same per-node latency averages).
	NodeStats []NodeStat `json:"node_stats,omitempty"`
	// Not replicated: details used by condition triggers on the evaluating node.
	StatusCodes   []int      `json:"-"`
	CertExpiresAt *time.Time `json:"-"`
}

// NodeStat is one node's probe in a round.
type NodeStat struct {
	NodeID    string   `json:"node_id"`
	OK        bool     `json:"ok"`
	LatencyMs *float64 `json:"latency_ms,omitempty"`
}

// IncidentStatus values for the incident state machine.
type IncidentStatus string

const (
	IncidentSuspected          IncidentStatus = "SUSPECTED"
	IncidentConfirmed          IncidentStatus = "CONFIRMED"
	IncidentAcknowledged       IncidentStatus = "ACKNOWLEDGED"
	IncidentRecoveryPending    IncidentStatus = "RECOVERY_PENDING"
	IncidentRecovering         IncidentStatus = "RECOVERING"
	IncidentFailedOver         IncidentStatus = "FAILED_OVER"
	IncidentResolved           IncidentStatus = "RESOLVED"
	IncidentManualIntervention IncidentStatus = "MANUAL_INTERVENTION"
	IncidentUnknown            IncidentStatus = "UNKNOWN"
)

type Incident struct {
	ID                  string         `json:"id"`
	MonitorID           string         `json:"monitor_id"`
	MonitorName         string         `json:"monitor_name,omitempty"`
	Status              IncidentStatus `json:"status"`
	Title               string         `json:"title"`
	StartedAt           time.Time      `json:"started_at"`
	DetectedAt          time.Time      `json:"detected_at"`
	ConfirmedAt         *time.Time     `json:"confirmed_at,omitempty"`
	AcknowledgedAt      *time.Time     `json:"acknowledged_at,omitempty"`
	AcknowledgedBy      string         `json:"acknowledged_by,omitempty"`
	ResolvedAt          *time.Time     `json:"resolved_at,omitempty"`
	Resolution          string         `json:"resolution"`
	ConfirmationSources []string       `json:"confirmation_sources"`
	LastErrorCategory   string         `json:"last_error_category"`
	UpdatedAt           time.Time      `json:"updated_at"`
}

type IncidentEvent struct {
	ID         string         `json:"id"`
	IncidentID string         `json:"incident_id"`
	At         time.Time      `json:"at"`
	Type       string         `json:"type"`
	FromStatus IncidentStatus `json:"from_status,omitempty"`
	ToStatus   IncidentStatus `json:"to_status,omitempty"`
	Actor      string         `json:"actor"`
	Message    string         `json:"message"`
	Details    map[string]any `json:"details,omitempty"`
}

type MaintenanceWindow struct {
	ID        string    `json:"id"`
	MonitorID string    `json:"monitor_id,omitempty"` // empty = all monitors
	Title     string    `json:"title"`
	StartsAt  time.Time `json:"starts_at"`
	EndsAt    time.Time `json:"ends_at"`
	CreatedBy string    `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
}

type AuditEntry struct {
	ID         string         `json:"id"`
	At         time.Time      `json:"at"`
	ActorID    string         `json:"actor_id"`
	ActorName  string         `json:"actor_name"`
	Action     string         `json:"action"`
	TargetType string         `json:"target_type"`
	TargetID   string         `json:"target_id"`
	IP         string         `json:"ip"`
	Outcome    string         `json:"outcome"`
	Details    map[string]any `json:"details,omitempty"`
}

// EnrollmentToken is a single-use, short-lived cluster join credential. Only a hash of the
// secret is stored.
type EnrollmentToken struct {
	ID          string     `json:"id"`
	SecretHash  string     `json:"-"`
	Description string     `json:"description"`
	CreatedBy   string     `json:"created_by"`
	CreatedAt   time.Time  `json:"created_at"`
	ExpiresAt   time.Time  `json:"expires_at"`
	UsedAt      *time.Time `json:"used_at,omitempty"`
	UsedByNode  string     `json:"used_by_node,omitempty"`
	RevokedAt   *time.Time `json:"revoked_at,omitempty"`
}

// ClusterNode is replicated display metadata about a cluster member.
type ClusterNode struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	ClusterAddr string    `json:"cluster_addr"`
	APIURL      string    `json:"api_url"`
	Location    string    `json:"location"`
	Version     string    `json:"version"`
	JoinedAt    time.Time `json:"joined_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// ---- automation (Phase 3) ----

// WorkflowTrigger defines when a workflow starts.
type WorkflowTrigger struct {
	Type string `json:"type"` // manual | incident | schedule | condition
	// incident trigger
	OnStatuses []IncidentStatus `json:"on_statuses,omitempty"`
	MonitorIDs []string         `json:"monitor_ids,omitempty"`
	GroupIDs   []string         `json:"group_ids,omitempty"`
	Tags       []string         `json:"tags,omitempty"`
	// schedule trigger
	IntervalSeconds int `json:"interval_seconds,omitempty"`
	// condition trigger: runs when a monitor's metric crosses a limit (uses the monitor filters
	// above). Metric: latency_ms | status_code | uptime_24h | cert_days | status.
	Metric    string  `json:"metric,omitempty"`
	Op        string  `json:"op,omitempty"` // gt | ge | lt | le | eq | ne
	Threshold float64 `json:"threshold,omitempty"`
	// StatusValue is compared for metric "status" (UP, DOWN, DEGRADED, ...).
	StatusValue string `json:"status_value,omitempty"`
	// RepeatMinutes is the minimum time between two runs for the same monitor (default 60).
	RepeatMinutes int `json:"repeat_minutes,omitempty"`
}

// WorkflowStep is one step of a workflow.
type WorkflowStep struct {
	Name           string `json:"name"`
	Type           string `json:"type"` // script | http | wait | incident_status
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
	OnFailure      string `json:"on_failure,omitempty"` // stop (default) | continue
	Retries        int    `json:"retries,omitempty"`    // retries after a definite failure only

	// script
	Language string            `json:"language,omitempty"` // bash | python
	Script   string            `json:"script,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
	// http
	Method              string            `json:"method,omitempty"`
	URL                 string            `json:"url,omitempty"`
	Headers             map[string]string `json:"headers,omitempty"`
	Body                string            `json:"body,omitempty"`
	ExpectedStatus      []string          `json:"expected_status,omitempty"`
	AllowPrivateNetwork bool              `json:"allow_private_network,omitempty"`
	// wait
	Seconds int `json:"seconds,omitempty"`
	// incident_status
	Status IncidentStatus `json:"status,omitempty"`
	Note   string         `json:"note,omitempty"`
	// dns
	ProviderID string `json:"provider_id,omitempty"`
	ZoneID     string `json:"zone_id,omitempty"`
	ZoneName   string `json:"zone_name,omitempty"`
	DNSAction  string `json:"dns_action,omitempty"` // upsert | delete
	RecordType string `json:"record_type,omitempty"`
	RecordName string `json:"record_name,omitempty"`
	Content    string `json:"content,omitempty"`
	TTL        int    `json:"ttl,omitempty"`
	Proxied    bool   `json:"proxied,omitempty"`
}

// Workflow is an automation definition.
type Workflow struct {
	ID          string          `json:"id"`
	Kind        string          `json:"kind,omitempty"` // "workflow" or "dr" (the backing row of a DR plan)
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Enabled     bool            `json:"enabled"`
	Trigger     WorkflowTrigger `json:"trigger"`
	Steps       []WorkflowStep  `json:"steps"`
	SecretBlob  []byte          `json:"-"`
	SecretKeys  []string        `json:"secret_keys"`
	// OperatorCanRun lets users with the operator role start this workflow manually.
	OperatorCanRun bool       `json:"operator_can_run"`
	Version        int        `json:"version"`
	NextFireAt     *time.Time `json:"next_fire_at,omitempty"`
	CreatedBy      string     `json:"created_by"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

// Run statuses.
const (
	RunPending        = "pending"
	RunRunning        = "running"
	RunWaiting        = "waiting"
	RunSucceeded      = "succeeded"
	RunFailed         = "failed"
	RunCancelled      = "cancelled"
	RunUnknownOutcome = "unknown_outcome" // an action's result is uncertain; needs reconciliation
)

// Action statuses.
const (
	ActionDispatching         = "dispatching"
	ActionSucceeded           = "succeeded"
	ActionFailed              = "failed"
	ActionUnknownOutcome      = "unknown_outcome"
	ActionReconciledSucceeded = "reconciled_succeeded"
	ActionReconciledFailed    = "reconciled_failed"
)

// WorkflowRun is one execution of a workflow.
type WorkflowRun struct {
	ID              string            `json:"id"`
	WorkflowID      string            `json:"workflow_id"`
	WorkflowName    string            `json:"workflow_name,omitempty"`
	WorkflowVersion int               `json:"workflow_version"`
	Steps           []WorkflowStep    `json:"steps"`
	TriggerType     string            `json:"trigger_type"`
	TriggerRef      string            `json:"trigger_ref"`
	Context         map[string]string `json:"context"`
	Status          string            `json:"status"`
	CurrentStep     int               `json:"current_step"`
	NextAt          *time.Time        `json:"next_at,omitempty"`
	CreatedBy       string            `json:"created_by"`
	CreatedAt       time.Time         `json:"created_at"`
	StartedAt       *time.Time        `json:"started_at,omitempty"`
	FinishedAt      *time.Time        `json:"finished_at,omitempty"`
	Error           string            `json:"error"`
	UpdatedAt       time.Time         `json:"updated_at"`
}

// WorkflowAction is the durable record of one external action dispatch (the fencing record).
type WorkflowAction struct {
	ID           string     `json:"id"`
	RunID        string     `json:"run_id"`
	StepIndex    int        `json:"step_index"`
	StepType     string     `json:"step_type"`
	Attempt      int        `json:"attempt"`
	Status       string     `json:"status"`
	FenceTerm    uint64     `json:"fence_term"`
	FenceLeader  string     `json:"fence_leader"`
	StartedAt    time.Time  `json:"started_at"`
	FinishedAt   *time.Time `json:"finished_at,omitempty"`
	ExitCode     *int       `json:"exit_code,omitempty"`
	Output       string     `json:"output"`
	Error        string     `json:"error"`
	ReconciledBy string     `json:"reconciled_by,omitempty"`
}

// DNSProvider is a configured DNS provider account. Credentials are write-only.
type DNSProvider struct {
	ID             string     `json:"id"`
	Name           string     `json:"name"`
	Type           string     `json:"type"` // cloudflare | arvancloud
	CredentialBlob []byte     `json:"-"`
	CreatedBy      string     `json:"created_by"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	LastVerifiedAt *time.Time `json:"last_verified_at,omitempty"`
	LastError      string     `json:"last_error"`
}

// DNSRecord is a stored copy of a provider record (snapshots and change history).
type DNSRecord struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	TTL     int    `json:"ttl"`
	Proxied bool   `json:"proxied"`
}

// DNSSnapshot is the state of a zone's records at a point in time.
type DNSSnapshot struct {
	ID          string      `json:"id"`
	ProviderID  string      `json:"provider_id"`
	ZoneID      string      `json:"zone_id"`
	ZoneName    string      `json:"zone_name"`
	Reason      string      `json:"reason"`
	Records     []DNSRecord `json:"records,omitempty"`
	RecordCount int         `json:"record_count"`
	CreatedBy   string      `json:"created_by"`
	CreatedAt   time.Time   `json:"created_at"`
}

// DNS change statuses.
const (
	DNSChangeDispatching          = "dispatching"
	DNSChangeApplied              = "applied"
	DNSChangeFailed               = "failed"
	DNSChangeUnknown              = "unknown_outcome"
	DNSChangeReconciledApplied    = "reconciled_applied"
	DNSChangeReconciledNotApplied = "reconciled_not_applied"
)

// DNSChange is the durable record of one record change (also its fencing record).
type DNSChange struct {
	ID          string     `json:"id"`
	ProviderID  string     `json:"provider_id"`
	ZoneID      string     `json:"zone_id"`
	ZoneName    string     `json:"zone_name"`
	Action      string     `json:"action"` // create | update | delete
	RecordType  string     `json:"record_type"`
	RecordName  string     `json:"record_name"`
	RecordID    string     `json:"record_id"`
	Before      *DNSRecord `json:"before,omitempty"`
	After       *DNSRecord `json:"after,omitempty"`
	Status      string     `json:"status"`
	Verified    bool       `json:"verified"`
	SnapshotID  string     `json:"snapshot_id"`
	Source      string     `json:"source"`
	FenceTerm   uint64     `json:"fence_term"`
	FenceLeader string     `json:"fence_leader"`
	Actor       string     `json:"actor"`
	Error       string     `json:"error"`
	CreatedAt   time.Time  `json:"created_at"`
	FinishedAt  *time.Time `json:"finished_at,omitempty"`
}

// ---- disaster recovery (Phase 5) ----

// DR plan states.
const (
	DRPrimary     = "primary"      // traffic goes to the primary
	DRFailingOver = "failing_over" // a failover run is in progress
	DRFailedOver  = "failed_over"  // traffic goes to the secondary
	DRFailingBack = "failing_back" // a failback run is in progress
	DRAttention   = "attention"    // a run failed or its outcome is unknown; an operator must decide
)

// DRConfig defines how a service fails over between a primary and a secondary.
type DRConfig struct {
	// MonitorIDs watch the primary. A confirmed incident on any of them can trigger an automatic
	// failover (when Automatic is set); all must be UP before a failback.
	MonitorIDs []string `json:"monitor_ids"`
	// SecondaryMonitorID, when set, must be UP before failing over.
	SecondaryMonitorID string `json:"secondary_monitor_id,omitempty"`
	// Automatic enables failover without a human (opt-in; off by default). Failback is always manual.
	Automatic       bool `json:"automatic"`
	CooldownSeconds int  `json:"cooldown_seconds"`
	// The DNS record that is switched.
	ProviderID     string `json:"provider_id"`
	ZoneName       string `json:"zone_name"`
	RecordType     string `json:"record_type"`
	RecordName     string `json:"record_name"`
	PrimaryValue   string `json:"primary_value"`
	SecondaryValue string `json:"secondary_value"`
	TTL            int    `json:"ttl"`
	Proxied        bool   `json:"proxied"`
	// Optional steps around the DNS switch during failover (script, http, wait).
	PreSteps  []WorkflowStep `json:"pre_steps"`
	PostSteps []WorkflowStep `json:"post_steps"`
}

// DRPlan is a disaster-recovery plan and its current state.
type DRPlan struct {
	ID               string     `json:"id"`
	Name             string     `json:"name"`
	Description      string     `json:"description"`
	Config           DRConfig   `json:"config"`
	SecretKeys       []string   `json:"secret_keys"`
	State            string     `json:"state"`
	ActiveRunID      string     `json:"active_run_id"`
	Direction        string     `json:"direction"`
	StateNote        string     `json:"state_note"`
	LastFailoverAt   *time.Time `json:"last_failover_at,omitempty"`
	LastTransitionAt time.Time  `json:"last_transition_at"`
	Version          int        `json:"version"`
	CreatedBy        string     `json:"created_by"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
}
