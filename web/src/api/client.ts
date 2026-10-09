import type {
  ApprovalPayload,
  CreateResult,
  FsFileResponse,
  FsListResponse,
  LogPage,
  SessionDetail,
  SessionParams,
  SessionRow,
} from '../types'

// Token resolution: `divvy serve` prints a URL with ?token=...; capture it
// once and keep it for the tab's lifetime. sessionStorage survives reloads
// but never leaves the machine like a cookie jar would.
const TOKEN_KEY = 'divvy.token'

export function initToken(): string {
  const fromURL = new URLSearchParams(window.location.search).get('token')
  if (fromURL) {
    sessionStorage.setItem(TOKEN_KEY, fromURL)
    // Clean the address bar without reloading.
    window.history.replaceState({}, '', window.location.pathname)
    return fromURL
  }
  return sessionStorage.getItem(TOKEN_KEY) ?? ''
}

export class ApiError extends Error {
  constructor(
    public status: number,
    message: string,
  ) {
    super(message)
  }
}

let token = initToken()

export function getToken(): string {
  return token
}

async function request<T>(method: string, path: string, body?: unknown): Promise<T> {
  const res = await fetch(`/api${path}`, {
    method,
    headers: {
      ...(body !== undefined ? { 'Content-Type': 'application/json' } : {}),
      Authorization: `Bearer ${token}`,
    },
    body: body !== undefined ? JSON.stringify(body) : undefined,
  })
  const text = await res.text()
  let parsed: unknown = null
  if (text) {
    try {
      parsed = JSON.parse(text)
    } catch {
      parsed = { error: text }
    }
  }
  if (!res.ok) {
    const msg = (parsed as { error?: string } | null)?.error ?? `${res.status} ${res.statusText}`
    throw new ApiError(res.status, msg)
  }
  return parsed as T
}

export const api = {
  health: () => request<{ status: string }>('GET', '/health'),

  listSessions: () =>
    request<{ sessions: SessionRow[] }>('GET', '/sessions').then((r) => r.sessions),

  createSession: (params: SessionParams) =>
    request<CreateResult>('POST', '/sessions', params),

  getSession: (id: string) => request<SessionDetail>('GET', `/sessions/${id}`),

  resumeSession: (id: string, params: Partial<SessionParams> = {}) =>
    request<CreateResult>('POST', `/sessions/${id}/resume`, params),

  planApprove: (id: string) =>
    request<{ ok: boolean }>('POST', `/sessions/${id}/plan/approve`, {}),
  planAdjust: (id: string, comment: string) =>
    request<{ ok: boolean }>('POST', `/sessions/${id}/plan/adjust`, { comment }),

  abort: (id: string) => request<{ ok: boolean }>('POST', `/sessions/${id}/abort`, {}),
  pause: (id: string) => request<{ ok: boolean }>('POST', `/sessions/${id}/pause`, {}),
  add: (id: string, instruction: string) =>
    request<{ ok: boolean }>('POST', `/sessions/${id}/add`, { instruction }),
  redo: (id: string, nodeId: string) =>
    request<{ ok: boolean }>('POST', `/sessions/${id}/redo`, { node_id: nodeId }),
  answer: (id: string, text: string) =>
    request<{ ok: boolean }>('POST', `/sessions/${id}/answer`, { text }),

  getApproval: (id: string, leafId: string) =>
    request<ApprovalPayload>('GET', `/sessions/${id}/approvals/${leafId}`),
  decideApproval: (id: string, leafId: string, decision: 'approve' | 'reject', comment: string) =>
    request<{ ok: boolean }>('POST', `/sessions/${id}/approvals/${leafId}`, {
      decision,
      comment,
    }),

  // Read-only workspace browser. session scopes the listing to that
  // session's workdir; omit it to use the server default.
  fsList: (path: string, session?: string) =>
    request<FsListResponse>(
      'GET',
      `/fs/list?path=${encodeURIComponent(path)}${session ? `&session=${encodeURIComponent(session)}` : ''}`,
    ),
  fsFile: (path: string, session?: string) =>
    request<FsFileResponse>(
      'GET',
      `/fs/file?path=${encodeURIComponent(path)}${session ? `&session=${encodeURIComponent(session)}` : ''}`,
    ),

  log: (id: string, offset: number, limit: number) =>
    request<LogPage>(
      'GET',
      `/sessions/${id}/log?offset=${offset}&limit=${limit}`,
    ),
}
