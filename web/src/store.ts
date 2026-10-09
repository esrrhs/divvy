import { create } from 'zustand'
import { api } from './api/client'
import { EventSourceClient, type SseStatus } from './api/sse'
import type {
  PhaseSnapshot,
  RunPhase,
  SessionParams,
  SessionRow,
  StreamEvent,
  TaskTree,
} from './types'

// eventViewCap bounds how many events the timeline keeps in memory for the
// 10k-event NFR; the event stream itself stays complete on disk.
const eventViewCap = 1000

interface AppState {
  // connectivity & directory
  sseStatus: SseStatus
  sessions: SessionRow[]
  sessionsLoading: boolean
  currentId: string | null
  live: PhaseSnapshot | null
  tree: TaskTree | null
  selectedNodeId: string | null
  // Read-only file currently shown in the center pane (null = session view).
  fileView: { sessionId: string | null; path: string } | null
  events: StreamEvent[]
  apiError: string | null

  // lifecycle
  refreshSessions: () => Promise<void>
  createSession: (params: SessionParams) => Promise<string>
  openSession: (id: string) => Promise<void>
  resumeSession: (id: string) => Promise<boolean>
  selectNode: (id: string | null) => void
  openFile: (path: string, sessionId?: string | null) => void
  closeFile: () => void
  clearError: () => void

  // Guided-run control actions. Each surfaces failures through actionError
  // (toast/inline) and returns success so callers can reset their form.
  planApprove: () => Promise<boolean>
  planAdjust: (comment: string) => Promise<boolean>
  abort: () => Promise<boolean>
  pause: () => Promise<boolean>
  addInstruction: (instruction: string) => Promise<boolean>
  redoNode: (nodeId: string) => Promise<boolean>
  answer: (text: string) => Promise<boolean>
  decideLeaf: (leafId: string, decision: 'approve' | 'reject', comment: string) => Promise<boolean>
  actionError: string | null
  clearActionError: () => void

  // SSE
  connect: (id: string, token: string) => void
  disconnect: () => void
  _sse: EventSourceClient | null
}

export const useStore = create<AppState>((set, get) => ({
  sseStatus: 'closed',
  sessions: [],
  sessionsLoading: false,
  currentId: null,
  live: null,
  tree: null,
  selectedNodeId: null,
  fileView: null,
  events: [],
  apiError: null,
  actionError: null,
  _sse: null,

  clearError: () => set({ apiError: null }),
  clearActionError: () => set({ actionError: null }),

  refreshSessions: async () => {
    set({ sessionsLoading: true })
    try {
      const sessions = await api.listSessions()
      set({ sessions, sessionsLoading: false })
    } catch (e) {
      set({ sessionsLoading: false, apiError: errorText(e) })
    }
  },

  createSession: async (params) => {
    const res = await api.createSession(params)
    await get().refreshSessions()
    await get().openSession(res.session_id)
    return res.session_id
  },

  openSession: async (id) => {
    if (!id) {
      set({ currentId: null, live: null, tree: null, selectedNodeId: null, fileView: null, events: [] })
      return
    }
    set({ currentId: id, events: [], live: null, tree: null, selectedNodeId: null, fileView: null })
    get().disconnect()
    try {
      const detail = await api.getSession(id)
      set({
        live: detail.live ?? null,
        tree: (detail.tree as TaskTree | null) ?? null,
        selectedNodeId: (detail.tree as TaskTree | null)?.root_id ?? null,
      })
    } catch (e) {
      // A historical (finished, non-live) session has no live object; that
      // is not an error. Only surface real failures.
      set({ apiError: errorText(e) })
    }
  },

  resumeSession: async (id) => {
    try {
      // Parameters are optional: the backend fills model/baseURL/key from
      // its env-driven defaults. The resumed run parks at plan review again.
      const res = await api.resumeSession(id, {})
      await get().refreshSessions()
      await get().openSession(res.session_id)
      set({ actionError: null })
      return true
    } catch (e) {
      set({ actionError: errorText(e) })
      return false
    }
  },

  selectNode: (id) => set({ selectedNodeId: id }),

  openFile: (path, sessionId) =>
    set({ fileView: { path, sessionId: sessionId ?? get().currentId } }),
  closeFile: () => set({ fileView: null }),

  connect: (id, t) => {
    get().disconnect()
    const sse = new EventSourceClient(id, t, {
      onStatus: (sseStatus) => set({ sseStatus }),
      onEvent: (ev) => {
        // Phase snapshots piggyback on the same stream the UI will render
        // from (Task 8 consumes the tree_snapshot payloads fully).
        set((s) => {
          const events =
            s.events.length >= eventViewCap
              ? [...s.events.slice(s.events.length - eventViewCap + 1), ev]
              : [...s.events, ev]
          let live = s.live
          let tree = s.tree
          let selectedNodeId = s.selectedNodeId

          // The authoritative tree replaces the previous view; keep the
          // user's selection if the node still exists.
          if (ev.kind === 'tree_snapshot' && ev.tree && typeof ev.tree === 'object') {
            tree = ev.tree as TaskTree
            if (selectedNodeId && !(selectedNodeId in tree.nodes)) {
              selectedNodeId = tree.root_id
            }
            if (!selectedNodeId) selectedNodeId = tree.root_id
          } else if (ev.kind === 'plan_review' && ev.phase === 'pending') {
            live = withPhase(live, id, 'plan_review')
          } else if (ev.kind === 'leaf_approval') {
            const phase = ev.phase
            live =
              phase === 'pending'
                ? withPhase(live, id, 'leaf_approval', {
                    leaf_approving: String(ev.node_id ?? live?.leaf_approving ?? ''),
                  })
                : withPhase(live, id, 'running', { leaf_approving: '' })
          } else if (ev.kind === 'ask_pending') {
            live = withPhase(live, id, 'ask', { pending_ask: String(ev.question ?? '') })
          } else if (ev.kind === 'ask') {
            // The question was answered and the worker is continuing.
            live = withPhase(live, id, 'running', { pending_ask: '' })
          } else if (ev.kind === 'state_change' && (ev.to === 'RUNNING' || ev.to === 'VERIFYING')) {
            live = withPhase(live, id, 'running', { pending_ask: '' })
          } else if (ev.kind === 'session_end') {
            const o = String(ev.outcome ?? '')
            const next: RunPhase =
              o === 'completed' ? 'done' : o === 'paused' || o === 'interrupted' ? 'paused' : 'failed'
            live = withPhase(live, id, next)
          } else if (ev.kind === 'user_pause') {
            live = withPhase(live, id, 'paused')
          }
          return { events, live, tree, selectedNodeId }
        })
      },
    })
    sse.start()
    set({ _sse: sse })
  },

  disconnect: () => {
    const sse = get()._sse
    if (sse) {
      sse.close()
      set({ _sse: null, sseStatus: 'closed' })
    }
  },

  planApprove: async () => withRun(set, get, (id) => api.planApprove(id)),
  planAdjust: async (comment) =>
    withRun(set, get, (id) => api.planAdjust(id, comment)),
  abort: async () => withRun(set, get, (id) => api.abort(id)),
  pause: async () => withRun(set, get, (id) => api.pause(id)),
  addInstruction: async (instruction) =>
    withRun(set, get, (id) => api.add(id, instruction)),
  redoNode: async (nodeId) => withRun(set, get, (id) => api.redo(id, nodeId)),
  answer: async (text) => withRun(set, get, (id) => api.answer(id, text)),
  decideLeaf: async (leafId, decision, comment) =>
    withRun(set, get, (id) => api.decideApproval(id, leafId, decision, comment)),
}))

async function withRun(
  set: (p: Partial<AppState>) => void,
  get: () => AppState,
  fn: (id: string) => Promise<unknown>,
): Promise<boolean> {
  const id = get().currentId
  if (!id) {
    set({ actionError: '没有打开的会话' })
    return false
  }
  try {
    await fn(id)
    set({ actionError: null })
    return true
  } catch (e) {
    set({ actionError: errorText(e) })
    return false
  }
}

function withPhase(
  live: PhaseSnapshot | null,
  id: string,
  phase: RunPhase,
  extra: Partial<PhaseSnapshot> = {},
): PhaseSnapshot {
  return {
    session_id: id,
    workdir: live?.workdir ?? '',
    approval_mode: live?.approval_mode ?? 'auto',
    ...live,
    phase,
    ...extra,
  }
}

function errorText(e: unknown): string {
  if (e instanceof Error) return e.message
  return String(e)
}
