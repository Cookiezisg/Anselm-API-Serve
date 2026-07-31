// API client — the single fetch boundary to the Go dashboard backend.
//
// It carries NO credential and NO CSRF token: the backend listens on loopback
// only and a preceding IAP is the authentication boundary. Every failure uses
// the common API envelope.
//
// 本客户端**不携带**任何凭证与 CSRF token:后端只监听 loopback,鉴权边界是前置的 IAP。
// 所有失败都走同一个错误信封。

import type {
  OverviewResponse,
  ConfigResponse,
	InstallsResponse,
	AuditResponse,
	QuotaResetResponse,
} from './types'

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
}

async function request<T>(path: string, opts: RequestOptions = {}): Promise<T> {
  const { method = 'GET', body } = opts
  const headers: Record<string, string> = {}
  if (body !== undefined) {
    headers['Content-Type'] = 'application/json'
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
  return request<ConfigResponse>('/api/config', { method: 'POST', body: overrides })
}

export function banInstall(installId: string, reason: string): Promise<{ install_id: string; status: string }> {
  return request('/api/installs/ban', {
    method: 'POST',
    body: { install_id: installId, reason },
  })
}

export function unbanInstall(installId: string): Promise<{ install_id: string; status: string }> {
  return request('/api/installs/unban', {
    method: 'POST',
    body: { install_id: installId },
  })
}

export function resetAllMonthlyQuota(reason: string): Promise<QuotaResetResponse> {
	return request<QuotaResetResponse>('/api/quota/reset', {
		method: 'POST',
		body: { reason },
	})
}

// exportUrl is the GET /api/export download href (a plain anchor navigation so the
// browser handles the attachment; the session cookie rides same-origin).
export const exportUrl = '/api/export'
