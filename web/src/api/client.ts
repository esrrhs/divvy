import type {
  ApprovalPayload,
  ApplyHunkRequest,
  AiEditApplyRequest,
  AiEditApplyResponse,
  AiEditRequest,
  AiEditResponse,
  AiEditUndoResponse,
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

  aiEdit: (id: string, body: AiEditRequest) =>
    request<AiEditResponse>('POST', `/conversations/${id}/ai-edit`, body),

  // aiEditStream consumes the SSE form of the same endpoint so the dialog can
  // show code as the model produces it. Resolves with the parsed result carried
  // by the `done` event; `onDelta` receives raw code chunks.
  aiEditStream: async (
    id: string,
    body: AiEditRequest,
    onDelta: (text: string) => void,
    signal?: AbortSignal,
  ): Promise<AiEditResponse> => {
    const res = await fetch(`/api/conversations/${id}/ai-edit`, {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        Authorization: `Bearer ${token}`,
      },
      body: JSON.stringify({ ...body, stream: true }),
      signal,
    })
    if (!res.ok) {
      const text = await res.text()
      let msg = `${res.status} ${res.statusText}`
      try {
        const parsed = JSON.parse(text) as { error?: string }
        if (parsed?.error) msg = parsed.error
      } catch {
        if (text) msg = text
      }
      throw new ApiError(res.status, msg)
    }
    if (!res.body) throw new ApiError(0, '浏览器不支持流式响应')

    const reader = res.body.getReader()
    const decoder = new TextDecoder()
    let buf = ''
    let event = ''
    let done: AiEditResponse | null = null
    let failure: string | null = null

    const handleFrame = (frame: string) => {
      for (const line of frame.split('\n')) {
        if (line.startsWith('event: ')) {
          event = line.slice(7).trim()
        } else if (line.startsWith('data: ')) {
          const payload = line.slice(6)
          if (event === 'delta') {
            const d = JSON.parse(payload) as { text?: string }
            if (d.text) onDelta(d.text)
          } else if (event === 'done') {
            done = JSON.parse(payload) as AiEditResponse
          } else if (event === 'error') {
            const e = JSON.parse(payload) as { error?: string }
            failure = e.error ?? '生成失败'
          }
        }
      }
    }

    for (;;) {
      const { value, done: eof } = await reader.read()
      if (value) buf += decoder.decode(value, { stream: true })
      let sep = buf.indexOf('\n\n')
      while (sep >= 0) {
        const frame = buf.slice(0, sep)
        buf = buf.slice(sep + 2)
        handleFrame(frame)
        sep = buf.indexOf('\n\n')
      }
      if (eof) break
    }
    if (buf.trim()) handleFrame(buf)
    if (failure) throw new ApiError(502, failure)
    if (!done) throw new ApiError(502, '生成结束但没有收到结果')
    return done
  },

  aiEditApply: (id: string, body: AiEditApplyRequest) =>
    request<AiEditApplyResponse>('POST', `/conversations/${id}/ai-edit/apply`, body),

  aiEditUndo: (id: string) => request<AiEditUndoResponse>('POST', `/conversations/${id}/ai-edit/undo`, {}),
}
