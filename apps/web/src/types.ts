export type Role = 'owner' | 'admin' | 'operator' | 'viewer'
export type MonitorStatus = 'UP' | 'DOWN' | 'DEGRADED' | 'SUSPECTED_DOWN' | 'UNKNOWN' | 'PAUSED' | 'MAINTENANCE'
export type IncidentStatus = 'SUSPECTED' | 'CONFIRMED' | 'ACKNOWLEDGED' | 'RECOVERY_PENDING' | 'RECOVERING' | 'FAILED_OVER' | 'RESOLVED' | 'MANUAL_INTERVENTION' | 'UNKNOWN'
export type Verdict = 'ok' | 'partial' | 'failed' | 'inconclusive' | 'maintenance'

export interface User {
  id: string
  username: string
  display_name: string
  role: Role
  disabled: boolean
  language: 'en' | 'fa'
  created_at: string
  last_login_at?: string
}

export interface NodeInfo {
  node_id: string
  node_name: string
  role: 'STANDALONE' | 'FOLLOWER' | 'LEADER' | 'NO_QUORUM'
  can_manage: boolean
  version: string
  commit: string
  build_date: string
  started_at: string
  uptime_seconds?: number
  database_bytes?: number
  assigned_monitors?: number
}

export interface Me {
  user: User
  csrf_token: string
  permissions: string[]
  node: NodeInfo
}

export interface Policy {
  confirmations_required: number
  min_locations: number
  min_valid_probes: number
  fail_mode: 'consecutive' | 'window'
  failure_rounds: number
  window_seconds?: number
  recovery_rounds: number
  cooldown_seconds: number
}

export interface MonitorSettings {
  method?: string
  headers?: Record<string, string>
  body?: string
  expected_status?: string[]
  expected_body?: string
  follow_redirects?: boolean
  max_redirects?: number
  skip_tls_verify?: boolean
  origin_address?: string
  host_header?: string
  tls_server_name?: string
  degraded_latency_ms?: number
  cert_expiry_warn_days?: number
  cert_expiry_fail_days?: number
  record_type?: string
  expected_value?: string
  resolver?: string
  check_path?: string
}

export interface MonitorState {
  status: MonitorStatus
  consecutive_failed: number
  consecutive_ok: number
  last_round: number
  last_verdict: Verdict
  last_latency_ms?: number
  status_since: string
}

export interface Monitor {
  id: string
  name: string
  description: string
  type: 'http' | 'tcp' | 'dns' | 'tls'
  target: string
  group_id?: string
  group_name?: string
  tags: string[]
  interval_seconds: number
  timeout_seconds: number
  retries: number
  enabled: boolean
  settings: MonitorSettings
  secret_keys: string[]
  policy: Policy
  node_ids: string[]
  version: number
  created_at: string
  updated_at: string
  state?: MonitorState
  uptime_24h: number | null
}

export interface CheckResult {
  id: string
  node_id: string
  probe_at: string
  success: boolean
  slow: boolean
  latency_ms?: number
  http_status?: number
  error_category: string
  error_message: string
  attempts: number
  config_version: number
}

export interface UptimeStats {
  ok_rounds: number
  partial_rounds: number
  failed_rounds: number
  inconclusive_rounds: number
  maintenance_rounds: number
  expected_rounds: number
  avg_latency_ms?: number
  availability: number | null
  coverage: number | null
}

export interface DailyBucket extends UptimeStats { day: string }

export interface LatencyPoint { at: string; avg_ms: number; max_ms: number; failures: number; total: number }

export interface RoundResult {
  round_start: string
  verdict: Verdict
  valid_probes: number
  failing_probes: number
  failing_nodes: string[]
  avg_latency_ms?: number
}

export interface Incident {
  id: string
  monitor_id: string
  monitor_name?: string
  status: IncidentStatus
  title: string
  started_at: string
  detected_at: string
  confirmed_at?: string
  acknowledged_at?: string
  acknowledged_by?: string
  resolved_at?: string
  resolution: string
  confirmation_sources: string[]
  last_error_category: string
}

export interface IncidentEvent {
  id: string
  at: string
  type: string
  from_status?: string
  to_status?: string
  actor: string
  message: string
  details?: Record<string, unknown>
}

export interface Group { id: string; name: string; description: string; monitor_count?: number }

export interface MaintenanceWindow { id: string; monitor_id?: string; title: string; starts_at: string; ends_at: string; created_by: string }

export interface AuditEntry {
  id: string
  at: string
  actor_name: string
  action: string
  target_type: string
  target_id: string
  ip: string
  outcome: string
  details?: Record<string, unknown>
}

export interface Settings {
  instance_name: string
  default_language: 'en' | 'fa'
  raw_retention_days: number
  round_retention_days: number
  hourly_retention_days: number
}

export interface ClusterMember {
  id: string
  name: string
  address: string
  api_url: string
  location: string
  version: string
  suffrage: 'voter' | 'nonvoter'
  self: boolean
  leader: boolean
  reachable: boolean
  last_seen_ms: number | null
  applied_index: number
  raft_state: string
  outbox: number
}

export interface ClusterStatus {
  enabled: boolean
  cluster_id?: string
  node_id: string
  role: NodeInfo['role']
  raft_state?: string
  term?: string
  leader_id?: string
  leader_api_url?: string
  commit_index?: string
  applied_index?: number
  last_contact_ms?: number
  advertise?: string
  gate?: { open: boolean; voters: number; reachable: number; required: number }
  members: ClusterMember[]
  mesh?: Record<string, Record<string, boolean>>
  warnings: string[]
  defaults: { advertise?: string; listen?: string }
}

export interface EnrollmentToken {
  id: string
  description: string
  created_by: string
  created_at: string
  expires_at: string
  used_at?: string
  used_by_node?: string
  revoked_at?: string
}

export type StepType = 'script' | 'http' | 'wait' | 'incident_status' | 'dns'

export interface WorkflowStep {
  name: string
  type: StepType
  timeout_seconds?: number
  on_failure?: 'stop' | 'continue'
  retries?: number
  language?: 'bash' | 'python'
  script?: string
  env?: Record<string, string>
  method?: string
  url?: string
  headers?: Record<string, string>
  body?: string
  expected_status?: string[]
  allow_private_network?: boolean
  seconds?: number
  status?: IncidentStatus
  note?: string
  provider_id?: string
  zone_id?: string
  zone_name?: string
  dns_action?: 'upsert' | 'delete'
  record_type?: string
  record_name?: string
  content?: string
  ttl?: number
  proxied?: boolean
}

export interface WorkflowTrigger {
  type: 'manual' | 'incident' | 'schedule'
  on_statuses?: IncidentStatus[]
  monitor_ids?: string[]
  group_ids?: string[]
  tags?: string[]
  interval_seconds?: number
}

export interface Workflow {
  id: string
  name: string
  description: string
  enabled: boolean
  operator_can_run: boolean
  trigger: WorkflowTrigger
  steps: WorkflowStep[]
  secret_keys: string[]
  version: number
  next_fire_at?: string
  created_by: string
  created_at: string
  updated_at: string
}

export type RunStatus = 'pending' | 'running' | 'waiting' | 'succeeded' | 'failed' | 'cancelled' | 'unknown_outcome'
export type ActionStatus = 'dispatching' | 'succeeded' | 'failed' | 'unknown_outcome' | 'reconciled_succeeded' | 'reconciled_failed'

export interface WorkflowRun {
  id: string
  workflow_id: string
  workflow_name?: string
  workflow_version: number
  steps: WorkflowStep[]
  trigger_type: string
  trigger_ref: string
  context: Record<string, string>
  status: RunStatus
  current_step: number
  next_at?: string
  created_by: string
  created_at: string
  started_at?: string
  finished_at?: string
  error: string
  updated_at: string
}

export interface WorkflowAction {
  id: string
  run_id: string
  step_index: number
  step_type: StepType
  attempt: number
  status: ActionStatus
  fence_term: number
  fence_leader: string
  started_at: string
  finished_at?: string
  exit_code?: number
  output: string
  error: string
  reconciled_by?: string
}

export interface AutomationStatus {
  executes_here: boolean
  runner: { configured: boolean; reachable: boolean; interpreters: Record<string, boolean> }
  runs: Record<string, number>
}

// ---- DNS (Phase 4) ----

export type DNSProviderType = 'cloudflare' | 'arvancloud'

export interface DNSProvider {
  id: string
  name: string
  type: DNSProviderType
  created_by: string
  created_at: string
  updated_at: string
  last_verified_at?: string
  last_error: string
}

export interface DNSZone { id: string; name: string; status?: string }

export interface DNSRecord {
  id: string
  type: string
  name: string
  content: string
  ttl: number
  proxied: boolean
  editable?: boolean
}

export interface DNSRecordInput { type: string; name: string; content: string; ttl: number; proxied: boolean }

export interface DNSChangeRequest {
  provider_id: string
  zone: string
  action: 'upsert' | 'delete' | 'create'
  record_id?: string
  record: DNSRecordInput
}

export interface DNSPlan {
  provider_id: string
  zone: DNSZone
  action: 'create' | 'update' | 'delete'
  before?: DNSRecord
  after?: DNSRecord
  no_change: boolean
  warnings?: string[]
}

export type DNSChangeStatus = 'dispatching' | 'applied' | 'failed' | 'unknown_outcome' | 'reconciled_applied' | 'reconciled_not_applied'

export interface DNSChange {
  id: string
  provider_id: string
  zone_id: string
  zone_name: string
  action: 'create' | 'update' | 'delete'
  record_type: string
  record_name: string
  record_id: string
  before?: DNSRecord
  after?: DNSRecord
  status: DNSChangeStatus
  verified: boolean
  snapshot_id: string
  source: string
  fence_term: number
  fence_leader: string
  actor: string
  error: string
  created_at: string
  finished_at?: string
}

export interface DNSSnapshot {
  id: string
  provider_id: string
  zone_id: string
  zone_name: string
  reason: string
  records?: DNSRecord[]
  record_count: number
  created_by: string
  created_at: string
}

// ---- disaster recovery (Phase 5) ----

export type DRState = 'primary' | 'failing_over' | 'failed_over' | 'failing_back' | 'attention'

export interface DRConfig {
  monitor_ids: string[]
  secondary_monitor_id?: string
  automatic: boolean
  cooldown_seconds: number
  provider_id: string
  zone_name: string
  record_type: string
  record_name: string
  primary_value: string
  secondary_value: string
  ttl: number
  proxied: boolean
  pre_steps: WorkflowStep[]
  post_steps: WorkflowStep[]
}

export interface DRPlan {
  id: string
  name: string
  description: string
  config: DRConfig
  secret_keys: string[]
  state: DRState
  active_run_id: string
  direction: '' | 'failover' | 'failback'
  state_note: string
  last_failover_at?: string
  last_transition_at: string
  version: number
  created_by: string
  created_at: string
  updated_at: string
  waiting?: string
  run_status?: RunStatus
}

export interface DRCheck { id: string; ok: boolean; blocking: boolean; overridable: boolean; detail: string }

export interface DRResult { plan: DRPlan; checks: DRCheck[]; dns_plan?: DNSPlan; steps: string[]; run?: WorkflowRun }
