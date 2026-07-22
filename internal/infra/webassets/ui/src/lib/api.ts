// API client — the single fetch boundary to the Go dashboard backend.
//
// Contract anchors (internal/dashboard/*.go): builtin mode uses an HttpOnly
// session cookie + CSRF; external mode is authenticated by a preceding IAP and
// carries no Go session state. Every failure uses the common API envelope.

import type {
  LoginResponse,
  BootstrapResponse,
  DashboardAuthMode,
  OverviewResponse,
  ConfigResponse,
	InstallsResponse,
	AuditResponse,
	QuotaResetResponse,
} from './types'

// csrfToken lives ONLY in JS memory (never a cookie/localStorage): it is lost on
// F5 and re-fetched from GET /api/session, exactly as the backend intends.
let csrfToken = ''
let dashboardAuthMode: DashboardAuthMode | null = null

export function setCsrfToken(token: string): void {
  csrfToken = token
}

export function getCsrfToken(): string {
  return csrfToken
}

export function setDashboardAuthMode(mode: DashboardAuthMode): void {
  dashboardAuthMode = mode
}

// onUnauthorized is invoked when any /api call returns 401 (session gone): the
// app registers a hook that clears auth state + routes to #/login.
let onUnauthorized: (() => void) | null = null

export function setUnauthorizedHandler(fn: () => void): void {
  onUnauthorized = fn
}

// ApiError carries the parsed envelope so callers can branch on the stable code
// (e.g. CONFIG_REJECTED inline, LOGIN_LOCKED countdown) without string-matching.
export class ApiError extends Error {
  code: string
  status: number
  details?: Record<string, unknown>

  constructor(status: number, code: string, message: string, details?: Record<string, unknown>) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.code = code
    this.details = details
  }
}

interface RequestOptions {
  method?: string
  body?: unknown
  csrf?: boolean
}

async function request<T>(path: string, opts: RequestOptions = {}): Promise<T> {
  const { method = 'GET', body, csrf = false } = opts
  const headers: Record<string, string> = {}
  if (body !== undefined) {
    headers['Content-Type'] = 'application/json'
  }
  if (csrf && dashboardAuthMode === 'builtin') {
    headers['X-CSRF-Token'] = csrfToken
  }

  let resp: Response
  try {
    resp = await fetch(path, {
      method,
      credentials: 'same-origin',
      headers,
      body: body !== undefined ? JSON.stringify(body) : undefined,
    })
  } catch {
    // Network/transport failure — surface a synthetic envelope so the UI shows a
    // consistent message rather than an unhandled rejection.
    throw new ApiError(0, 'NETWORK_ERROR', '网络请求失败，请检查连接')
  }

  if (resp.status === 401) {
    // Session gone/expired: notify the app to clear state + route to login, then
    // still throw so the in-flight caller stops.
    if (onUnauthorized) {
      onUnauthorized()
    }
    const env = await safeErr(resp)
    throw new ApiError(401, env.code || 'UNAUTHENTICATED', env.message || '登录已失效', env.details)
  }

  if (!resp.ok) {
    const env = await safeErr(resp)
    throw new ApiError(resp.status, env.code || 'ERROR', env.message || `请求失败 (${resp.status})`, env.details)
  }

  // 204 / empty body → undefined.
  if (resp.status === 204) {
    return undefined as T
  }
  const text = await resp.text()
  if (!text) {
    return undefined as T
  }
  return JSON.parse(text) as T
}

// safeErr parses the failure envelope, tolerating a non-JSON body.
async function safeErr(resp: Response): Promise<{ code: string; message: string; details?: Record<string, unknown> }> {
  try {
    const data = await resp.json()
    if (data && typeof data === 'object' && 'error' in data) {
      const e = (data as { error: { code: string; message: string; details?: Record<string, unknown> } }).error
      return { code: e.code, message: e.message, details: e.details }
    }
  } catch {
    // fall through to a generic envelope
  }
  return { code: 'ERROR', message: `请求失败 (${resp.status})` }
}

// ── Auth ──────────────────────────────────────────────────────────────────────

// bootstrap is public on the loopback dashboard listener and contains only the
// active authentication contract, never a credential or user identity.
export function getBootstrap(): Promise<BootstrapResponse> {
  return request<BootstrapResponse>('/api/bootstrap')
}

// login posts {user,password}; the session cookie is set by the server and the
// returned csrfToken is stashed in memory for subsequent state-changing POSTs.
export async function login(user: string, password: string): Promise<LoginResponse> {
  const res = await request<LoginResponse>('/login', { method: 'POST', body: { user, password } })
  csrfToken = res.csrfToken
  return res
}

// recoverSession probes GET /api/session after an F5: a live cookie returns
// {user,csrfToken}, rehydrating the in-memory token; an invalid one 401s.
export async function recoverSession(): Promise<LoginResponse> {
  const res = await request<LoginResponse>('/api/session')
  csrfToken = res.csrfToken
  return res
}

export async function logout(): Promise<void> {
  // logout does NOT require CSRF (auth.go handleLogout); idempotent.
  await request<{ ok: boolean }>('/logout', { method: 'POST' })
  csrfToken = ''
}

// ── Read endpoints ──────────────────────────────────────────────────────────

export function getOverview(): Promise<OverviewResponse> {
  return request<OverviewResponse>('/api/overview')
}

export function getConfig(): Promise<ConfigResponse> {
  return request<ConfigResponse>('/api/config')
}

export function getInstalls(cursor: string, limit: number): Promise<InstallsResponse> {
  const q = new URLSearchParams()
  if (cursor) q.set('cursor', cursor)
  q.set('limit', String(limit))
  return request<InstallsResponse>(`/api/installs?${q.toString()}`)
}

export function getAudit(cursor: string, limit: number): Promise<AuditResponse> {
  const q = new URLSearchParams()
  if (cursor) q.set('cursor', cursor)
  q.set('limit', String(limit))
  return request<AuditResponse>(`/api/audit?${q.toString()}`)
}

// ── State-changing endpoints ─────────────────────────────────────────────────
// In builtin mode these carry the CSRF token; external mode has no Go browser
// credential, so the loopback listener trusts the preceding IAP instead.

export function postConfig(overrides: Record<string, string>): Promise<ConfigResponse> {
  return request<ConfigResponse>('/api/config', { method: 'POST', body: overrides, csrf: true })
}

export function banInstall(installId: string, reason: string): Promise<{ install_id: string; status: string }> {
  return request('/api/installs/ban', {
    method: 'POST',
    body: { install_id: installId, reason },
    csrf: true,
  })
}

export function unbanInstall(installId: string): Promise<{ install_id: string; status: string }> {
  return request('/api/installs/unban', {
    method: 'POST',
    body: { install_id: installId },
    csrf: true,
  })
}

export function resetAllMonthlyQuota(reason: string): Promise<QuotaResetResponse> {
	return request<QuotaResetResponse>('/api/quota/reset', {
		method: 'POST',
		body: { reason },
		csrf: true,
	})
}

// exportUrl is the GET /api/export download href (a plain anchor navigation so the
// browser handles the attachment; the session cookie rides same-origin).
export const exportUrl = '/api/export'
