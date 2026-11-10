import type {
  ApprovalPayload,
  ApplyHunkRequest,
  CreateResult,
  FsEntry,
  CodeSelection,
  FsFileResponse,
  FsListResponse,
  FsWriteResponse,
  ProjectSearchResponse,
  Conversation,
  FileRef,
  LogPage,
  MessageMode,
  SessionDetail,
  SessionParams,
  SessionRow,
  TurnChange,
  TurnChangesResponse,
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
    public body: Record<string, unknown> | null = null,
  ) {
    super(message)
  }
}

let token = typeof window !== 'undefined' ? initToken() : ''

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
    throw new ApiError(res.status, msg, parsed && typeof parsed === 'object' ? (parsed as Record<string, unknown>) : null)
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

  deleteSession: (id: string) =>
    request<{ ok: boolean; session_id: string; removed: string[]; bytes: number }>(
      'DELETE',
      `/sessions/${id}`,
    ),

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
  decideApproval: (
    id: string,
    leafId: string,
    decision: 'approve' | 'reject',
    comment: string,
    hunks?: Record<string, ApplyHunkRequest>,
  ) =>
    request<{ ok: boolean }>('POST', `/sessions/${id}/approvals/${leafId}`, {
      decision,
      comment,
      ...(hunks && Object.keys(hunks).length ? { hunks } : {}),
    }),

  // Workspace browser and text editor. session/conversation scopes the
  // listing to that workdir; omit them to use the server default.
  fsList: (path: string, session?: string, conversation?: string) =>
    request<FsListResponse>(
      'GET',
      `/fs/list?path=${encodeURIComponent(path)}${session ? `&session=${encodeURIComponent(session)}` : ''}${conversation ? `&conversation=${encodeURIComponent(conversation)}` : ''}`,
    ),
  fsFile: (path: string, session?: string, conversation?: string) =>
    request<FsFileResponse>(
      'GET',
      `/fs/file?path=${encodeURIComponent(path)}${session ? `&session=${encodeURIComponent(session)}` : ''}${conversation ? `&conversation=${encodeURIComponent(conversation)}` : ''}`,
    ),
  fsWrite: (
    path: string,
    content: string,
    baseHash: string,
    opts?: { session?: string; conversation?: string },
  ) =>
    request<FsWriteResponse>('PUT', '/fs/file', {
      path,
      content,
      base_hash: baseHash,
      ...(opts?.session ? { session: opts.session } : {}),
      ...(opts?.conversation ? { conversation: opts.conversation } : {}),
    }),

  fsSearch: (
    q: string,
    opts?: {
      session?: string
      conversation?: string
      regex?: boolean
      caseSensitive?: boolean
      glob?: string
      limit?: number
    },
  ) => {
    const p = new URLSearchParams({ q })
    if (opts?.session) p.set('session', opts.session)
    if (opts?.conversation) p.set('conversation', opts.conversation)
    if (opts?.regex) p.set('regex', '1')
    if (opts?.caseSensitive) p.set('case', '1')
    if (opts?.glob) p.set('glob', opts.glob)
    if (opts?.limit) p.set('limit', String(opts.limit))
    return request<ProjectSearchResponse>('GET', `/fs/search?${p.toString()}`)
  },

  log: (id: string, offset: number, limit: number) =>
    request<LogPage>(
      'GET',
      `/sessions/${id}/log?offset=${offset}&limit=${limit}`,
    ),

  browse: (path?: string) =>
    request<{ path: string; parent: string; entries: FsEntry[]; truncated: boolean }>(
      'GET',
      `/browse${path ? `?path=${encodeURIComponent(path)}` : ''}`,
    ),

  listConversations: () =>
    request<{ conversations: Conversation[] }>('GET', '/conversations').then((r) => r.conversations),

  createConversation: (params: SessionParams & { workdir: string; message?: string; title?: string }) =>
    request<Conversation>('POST', '/conversations', params),

  getConversation: (id: string) => request<Conversation>('GET', `/conversations/${id}`),

  deleteConversation: (id: string) => request<{ ok: boolean; id: string }>('DELETE', `/conversations/${id}`),

  postMessage: (
    id: string,
    text: string,
    opts?: { apiKey?: string; paths?: string[]; selections?: CodeSelection[]; mode?: MessageMode },
  ) =>
    request<Conversation>('POST', `/conversations/${id}/messages`, {
      text,
      ...(opts?.apiKey ? { api_key: opts.apiKey } : {}),
      ...(opts?.paths && opts.paths.length ? { paths: opts.paths } : {}),
      ...(opts?.selections && opts.selections.length ? { selections: opts.selections } : {}),
      ...(opts?.mode ? { mode: opts.mode } : {}),
    }),

  rollbackTurn: (id: string, sessionId: string) =>
    request<{ ok: boolean; session_id: string; restored: string[]; conversation?: Conversation }>(
      'POST',
      `/conversations/${id}/turns/${encodeURIComponent(sessionId)}/rollback`,
      { confirm: true },
    ),

  recoverRollback: (id: string) =>
    request<{
      ok: boolean
      completed?: boolean
      compensated?: boolean
      pending?: boolean
      restored?: string[]
      conflicts?: string[]
      error?: string
      conversation?: Conversation
    }>('POST', `/conversations/${id}/rollback/recover`),

  cancelQueue: (id: string) => request<Conversation>('DELETE', `/conversations/${id}/queue`),

  cancelSteer: (id: string) => request<Conversation>('DELETE', `/conversations/${id}/steer`),

  searchConversationFiles: (id: string, q: string, limit = 20) =>
    request<{ entries: FileRef[]; query: string }>(
      'GET',
      `/conversations/${id}/files?q=${encodeURIComponent(q)}&limit=${limit}`,
    ),

  getTurnChanges: (id: string, sessionId: string) =>
    request<TurnChangesResponse>(
      'GET',
      `/conversations/${id}/turns/${encodeURIComponent(sessionId)}/changes`,
    ),

  applyTurnHunks: (id: string, sessionId: string, body: ApplyHunkRequest) =>
    request<{
      session_id: string
      path: string
      applied: string
      changes: TurnChange[]
      editable?: boolean
      conversation?: Conversation
    }>('POST', `/conversations/${id}/turns/${encodeURIComponent(sessionId)}/changes`, body),

  setConversationKey: (id: string, apiKey: string) =>
    request<Conversation>('POST', `/conversations/${id}/key`, { api_key: apiKey }),

  resumeConversation: (id: string, apiKey?: string) =>
    request<Conversation>('POST', `/conversations/${id}/resume`, apiKey ? { api_key: apiKey } : {}),
}
