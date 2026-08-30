import type { ApiError } from './types'

let csrfToken: string | null = null

export function setCsrfToken(token: string | null): void {
  csrfToken = token
}

export class ApiFailure extends Error {
  code: string
  status: number
  requestId?: string

  constructor(status: number, err: ApiError) {
    super(err.message)
    this.code = err.code
    this.status = status
    this.requestId = err.request_id
  }
}

async function parseError(resp: Response): Promise<ApiFailure> {
  try {
    const body = await resp.json()
    if (body?.error) return new ApiFailure(resp.status, body.error)
  } catch {
    /* fall through */
  }
  return new ApiFailure(resp.status, { code: 'internal', message: `request failed (${resp.status})` })
}

function request<T>(
  method: string,
  path: string,
  body?: unknown,
  csrf = true,
  extra?: Record<string, string>
): Promise<T> {
  const headers: Record<string, string> = { ...extra }
  if (body !== undefined) headers['Content-Type'] = 'application/json'
  if (csrf && csrfToken && method !== 'GET') headers['X-CSRF-Token'] = csrfToken
  return fetch(path, {
    method,
    headers,
    body: body === undefined ? undefined : JSON.stringify(body),
    credentials: 'same-origin'
  }).then(async (resp) => {
    if (!resp.ok) throw await parseError(resp)
    const text = await resp.text()
    return text ? (JSON.parse(text) as T) : ({} as T)
  })
}

export function getCsrfToken(): string | null {
  return csrfToken
}

export type ClearScope = 'finished' | 'all'

export interface ClearResponse {
  scope: ClearScope
  removed: number
}

export const api = {
  // setupToken is only meaningful during first-run setup: it is what lets an
  // origin the server cannot recognise as local be issued a setup CSRF token.
  session: (setupToken?: string) =>
    request<import('./types').SessionInfo>(
      'GET',
      '/api/v1/session',
      undefined,
      true,
      setupToken ? { 'X-Setup-Token': setupToken } : undefined
    ),
  login: (password: string) =>
    request<import('./types').SessionInfo>(
      'POST',
      '/api/v1/session',
      { password },
      false
    ),
  logout: () => request<{ ok: boolean }>('DELETE', '/api/v1/session'),
  changePassword: (password: string, confirmation: string, setupToken?: string) =>
    request<import('./types').SessionInfo>(
      'PUT',
      '/api/v1/session/password',
      { password, confirmation },
      true,
      setupToken ? { 'X-Setup-Token': setupToken } : undefined
    ),

  listDownloads: (params?: { state?: string; search?: string }) => {
    const q = new URLSearchParams()
    if (params?.state) q.set('state', params.state)
    if (params?.search) q.set('search', params.search)
    const qs = q.toString()
    return request<{ downloads: import('./types').Download[] }>(
      'GET',
      '/api/v1/downloads' + (qs ? `?${qs}` : '')
    )
  },
  getDownload: (id: string) =>
    request<{ download: import('./types').Download }>('GET', `/api/v1/downloads/${encodeURIComponent(id)}`),
  addDownload: (url: string) =>
    request<import('./types').AddDownloadResponse>('POST', '/api/v1/downloads', { url, start_now: false }),

  batchAction: (action: string, ids: string[]) =>
    request<{ results: import('./types').BatchResult[] }>('POST', '/api/v1/downloads/actions', {
      action,
      ids
    }),

  clearQueue: (scope: ClearScope) =>
    request<ClearResponse>('POST', '/api/v1/downloads/clear', { scope }),

  openapi: () => request<Record<string, unknown>>('GET', '/api/v1/openapi.json'),

  settings: () => request<import('./types').SettingsView>('GET', '/api/v1/settings'),
  saveSettings: (patch: {
    ui?: { theme?: string; compact?: boolean }
    downloads?: { max_concurrent?: number }
  }) => request<import('./types').SettingsView>('PUT', '/api/v1/settings', patch),

  ytDlpSettings: () => request<import('./types').YtDlpSettingsResponse>('GET', '/api/v1/settings/yt-dlp'),
  saveYtDlpManaged: (settings: import('./types').YtDlpSettings) =>
    request<import('./types').YtDlpSettingsResponse>(
      'PUT',
      '/api/v1/settings/yt-dlp/managed',
      settings
    ),

  system: () => request<import('./types').SystemInfo>('GET', '/api/v1/system')
}
