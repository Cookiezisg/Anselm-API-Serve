// Wire types — a 1:1 mirror of the Go dashboard handlers (internal/dashboard/*.go).
// Field names/casings echo the json tags exactly so the SPA never guesses a shape.
//
// 后端契约的逐字投影:字段名/大小写与 Go handler 的 json tag 一一对应,前端不臆造。

// errBody (http.go): the {error:{code,message,details}} failure envelope.
export interface ErrBody {
  code: string
  message: string
  details?: Record<string, unknown>
}

// alert.AlertState (alert.go) surfaced inside overview.alerts.
export interface AlertState {
  reason: string
  severity: string
  firing: boolean
  message: string
  lastFiredAt?: string
}

// rates (rate.go) — recent-window derived view inside overview.recent.
export interface Rates {
  qps: number
  errorRate: number
  windowSec: number
}

// Closed provider state exposed by overview.providers. The object always has
// exactly the two deterministic routes; it never carries key counts or values.
export interface ProviderStatus {
  configured: boolean
  breakerOpen: boolean
}

export interface ProviderStatuses {
  qwen: ProviderStatus
}

// overviewResponse (api.go) — the live operational snapshot.
export interface OverviewResponse {
  budget: {
    day: string
    usedMicroUsd: number
    limitMicroUsd: number
    remainingMicroUsd: number
    unit: 'micro_usd'
  }
  inflightConcurrency: number
  openReservations: number
  providers: ProviderStatuses
  // Compatibility aggregate for older consumers; this SPA uses providers.
  upstreamBreakerOpen: boolean
  diskDegraded: boolean
  alerts: AlertState[] | null
  recent: Rates
  installsToday: number
  installGlobalCap: number
}

// config.DumpItem (config/runtime.go) — one config read-model row.
export interface DumpItem {
  key: string
  value: string
  editable: boolean
  restartRequired: boolean
  min?: number
  max?: number
}

// configResponse (api.go): {items:[...]}.
export interface ConfigResponse {
  items: DumpItem[]
}

// installRow (installs.go) — one install list row (opaque id only, no secret).
export interface InstallRow {
  id: string
  status: string
  createdAt: string
  lastSeenAt?: string
  todaySpendMicroUsd: number
}

// GET /api/installs response (installs.go).
export interface InstallsResponse {
  installs: InstallRow[] | null
  nextCursor: string
}

// AuditEvent (audit.go) — one audit ring record.
export interface AuditEvent {
  seq: number
  at: string
  action: string
  target: string
  reason: string
  outcome: string
  actor: string
}

// GET /api/audit response (api.go).
export interface AuditResponse {
	events: AuditEvent[] | null
	nextCursor: string
}

// POST /api/quota/reset response (dashboard handler): only the current reset
// month and an aggregate count are exposed; no install-level usage is returned.
export interface QuotaResetResponse {
	period: string
	resetInstalls: number
}
