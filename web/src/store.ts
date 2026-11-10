import { create } from 'zustand'
import { api, ApiError } from './api/client'
import { planFileOpen } from './lib/fileNav'
import { EventSourceClient, type SseStatus } from './api/sse'
import type {
  PhaseSnapshot,
  RunPhase,
  ApplyHunkRequest,
  CodeSelection,
  Conversation,
  SessionParams,
  SessionRow,
  StreamEvent,
  TaskTree,
  TurnChange,
} from './types'

// eventViewCap bounds how many events the timeline keeps in memory for the
// 10k-event NFR; the event stream itself stays complete on disk.
const eventViewCap = 1000

function selectionsFor(byConv: Record<string, CodeSelection[]>, convId: string | null): CodeSelection[] {
  if (!convId) return []
  return byConv[convId] ?? []
}

// Latest user-requested conversation. undefined means "follow currentConvId"
// (e.g. tests that only set the current id). openConversation always writes
// this before awaiting, so a slower earlier open cannot win the view.
function intendedConvId(s: { desiredConvId?: string | null; currentConvId: string | null }): string | null {
  return s.desiredConvId !== undefined ? s.desiredConvId : s.currentConvId
}

function isIntendedConv(s: { desiredConvId?: string | null; currentConvId: string | null }, id: string | null): boolean {
  return intendedConvId(s) === id
}

function mergeConversation(list: Conversation[], updated: Conversation): Conversation[] {
  const i = list.findIndex((c) => c.id === updated.id)
  if (i < 0) return [updated, ...list]
  const next = list.slice()
  next[i] = updated
  return next
}

function sameSelection(a: CodeSelection, b: CodeSelection): boolean {
  return (
    a.path === b.path &&
    a.start_line === b.start_line &&
    a.end_line === b.end_line &&
    a.text === b.text &&
    !!a.draft === !!b.draft
  )
}

/** Drop one copy of each sent citation, leaving anything attached after the send started. */
function withoutSent(list: CodeSelection[], sent: CodeSelection[]): CodeSelection[] {
  const remaining = list.slice()
  for (const item of sent) {
    const i = remaining.findIndex((x) => sameSelection(x, item))
    if (i >= 0) remaining.splice(i, 1)
  }
  return remaining
}

function beginConvNavigation(
  set: (partial: { desiredConvId: string | null; convNavGen: number; sessionNavGen: number }) => void,
  get: () => { convNavGen: number; sessionNavGen: number },
  convId: string | null,
): number {
  const cgen = get().convNavGen + 1
  const sgen = get().sessionNavGen + 1
  set({ desiredConvId: convId, convNavGen: cgen, sessionNavGen: sgen })
  return cgen
}

interface AppState {
  // connectivity & directory
  sseStatus: SseStatus
  sessions: SessionRow[]
  sessionsLoading: boolean
  conversations: Conversation[]
  conversationsLoading: boolean
  currentConvId: string | null
  conversation: Conversation | null
  // Intent of the latest openConversation (and similar navigations). Used to
  // ignore stale async results without clobbering a newer selection.
  desiredConvId?: string | null
  convNavGen: number
  sessionNavGen: number
  currentId: string | null
  live: PhaseSnapshot | null
  tree: TaskTree | null
  selectedNodeId: string | null
  // File currently shown in the center pane (null = conversation/session view).
  fileView: { sessionId: string | null; path: string; preferDiff?: boolean; line?: number; revealNonce?: number } | null
  fileDirty: boolean
  pendingFileNav: { path: string; sessionId: string | null; preferDiff?: boolean; line?: number; revealNonce?: number } | 'close' | null
  pendingSelections: CodeSelection[]
  pendingSelectionsByConv: Record<string, CodeSelection[]>
  turnChanges: { sessionId: string; changes: TurnChange[]; editable?: boolean; reason?: string } | null
  events: StreamEvent[]
  apiError: string | null

  // lifecycle
  refreshSessions: () => Promise<void>
  refreshConversations: () => Promise<void>
  createSession: (params: SessionParams) => Promise<string>
  createConversation: (params: SessionParams & { workdir: string; message?: string; title?: string }) => Promise<string>
  openConversation: (id: string) => Promise<void>
  postMessage: (text: string, opts?: { apiKey?: string; paths?: string[]; selections?: CodeSelection[]; mode?: 'start' | 'queue' | 'takeover' | 'steer' }) => Promise<boolean>
  cancelQueue: () => Promise<boolean>
  cancelSteer: () => Promise<boolean>
  loadTurnChanges: (sessionId?: string) => Promise<void>
  applyTurnHunks: (payload: ApplyHunkRequest, sessionId?: string) => Promise<boolean>
  rollbackTurn: (sessionId: string) => Promise<boolean>
  recoverRollback: () => Promise<boolean>
  resumeConversation: (apiKey?: string) => Promise<boolean>
  deleteConversation: (id: string) => Promise<boolean>
  setConversationKey: (apiKey: string) => Promise<boolean>
  openSession: (id: string, opts?: { convNavGen?: number }) => Promise<void>
  resumeSession: (id: string) => Promise<boolean>
  deleteSession: (id: string) => Promise<boolean>
  selectNode: (id: string | null) => void
  openFile: (path: string, sessionId?: string | null, opts?: { preferDiff?: boolean; line?: number }) => void
  closeFile: () => void
  setFileDirty: (dirty: boolean) => void
  confirmFileNav: () => void
  cancelFileNav: () => void
  attachSelection: (sel: CodeSelection, opts?: { conversationId?: string }) => void
  removeSelection: (index: number) => void
  clearSelections: () => void
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
  decideLeaf: (
    leafId: string,
    decision: 'approve' | 'reject',
    comment: string,
    hunks?: Record<string, ApplyHunkRequest>,
  ) => Promise<boolean>
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
  conversations: [],
  conversationsLoading: false,
  currentConvId: null,
  conversation: null,
  desiredConvId: undefined,
  convNavGen: 0,
  sessionNavGen: 0,
  currentId: null,
  live: null,
  tree: null,
  selectedNodeId: null,
  fileView: null,
  fileDirty: false,
  pendingFileNav: null,
  pendingSelections: [],
  pendingSelectionsByConv: {},
  turnChanges: null,
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

  refreshConversations: async () => {
    const intent = intendedConvId(get())
    set({ conversationsLoading: true })
    try {
      const conversations = await api.listConversations()
      set((s) => {
        // Only refresh the on-screen conversation when it is still the
        // intended one. An in-flight navigation must not be overwritten by a
        // list row for the previous or next id.
        const nextConv =
          s.currentConvId && isIntendedConv(s, s.currentConvId)
            ? conversations.find((c) => c.id === s.currentConvId) ?? s.conversation
            : s.conversation
        return { conversations, conversationsLoading: false, conversation: nextConv }
      })
    } catch (e) {
      if (intendedConvId(get()) === intent) {
        set({ conversationsLoading: false, apiError: errorText(e) })
      } else {
        set({ conversationsLoading: false })
      }
    }
  },

  createConversation: async (params) => {
    const nav = get().convNavGen
    const conv = await api.createConversation(params)
    await get().refreshConversations()
    // If the user opened another conversation while this create was in
    // flight, leave their selection alone; the new row is already in the list.
    if (get().convNavGen !== nav) return conv.id
    await get().openConversation(conv.id)
    return conv.id
  },

  openConversation: async (id) => {
    const convId = id || null
    // Bump session nav immediately so an in-flight getSession/openSession for
    // the previous conversation cannot write currentId/live after this click.
    const cgen = beginConvNavigation(set, get, convId)
    if (!convId) {
      set({ currentConvId: null, conversation: null, pendingSelections: [] })
      await get().openSession('', { convNavGen: cgen })
      return
    }
    try {
      const conversation = await api.getConversation(convId)
      if (get().convNavGen !== cgen || !isIntendedConv(get(), convId)) return
      set({
        currentConvId: convId,
        conversation,
        fileView: null,
        fileDirty: false,
        pendingFileNav: null,
        turnChanges: null,
        apiError: null,
        pendingSelections: selectionsFor(get().pendingSelectionsByConv, convId),
      })
      const lastSid = conversation.current_session_id || conversation.run_session_id || conversation.session_ids?.[conversation.session_ids.length - 1]
      if (lastSid) void get().loadTurnChanges(lastSid)
      const sid = conversation.current_session_id || conversation.run_session_id
      if (sid) {
        await get().openSession(sid, { convNavGen: cgen })
      } else {
        await get().openSession('', { convNavGen: cgen })
        // openSession('') also cleared currentConvId's sibling session state;
        // restore the conversation we just opened and its isolated citations.
        if (get().convNavGen !== cgen || !isIntendedConv(get(), convId)) return
        set({
          currentConvId: convId,
          conversation,
          pendingSelections: selectionsFor(get().pendingSelectionsByConv, convId),
        })
      }
    } catch (e) {
      if (get().convNavGen !== cgen || !isIntendedConv(get(), convId)) return
      set({ apiError: errorText(e) })
    }
  },

  postMessage: async (text, opts) => {
    const id = get().currentConvId
    if (!id) {
      set({ actionError: '没有打开的对话' })
      return false
    }
    try {
      const selections = opts?.selections ?? selectionsFor(get().pendingSelectionsByConv, id)
      const conversation = await api.postMessage(id, text, { ...opts, selections })
      const remaining = withoutSent(selectionsFor(get().pendingSelectionsByConv, id), selections)
      const byConv = { ...get().pendingSelectionsByConv }
      if (remaining.length) byConv[id] = remaining
      else delete byConv[id]
      const conversations = mergeConversation(get().conversations, conversation)
      // The send may already have succeeded for this conversation. Always
      // record it and drop only the drafts that were sent. If the user has
      // since opened another conversation, do not change the current
      // selection or clear the other conversation's drafts (or drafts added
      // to this conversation after the send started).
      if (!isIntendedConv(get(), id)) {
        set({
          conversations,
          pendingSelectionsByConv: byConv,
          pendingSelections: selectionsFor(byConv, get().currentConvId),
        })
        await get().refreshConversations()
        // Re-merge in case the list snapshot was stale or empty.
        set({ conversations: mergeConversation(get().conversations, conversation) })
        return true
      }
      set({
        conversation,
        currentConvId: conversation.id,
        conversations,
        actionError: null,
        pendingSelectionsByConv: byConv,
        pendingSelections: selectionsFor(byConv, conversation.id),
      })
      await get().refreshConversations()
      set({ conversations: mergeConversation(get().conversations, conversation) })
      if (!isIntendedConv(get(), id)) return true
      const sid = conversation.current_session_id || conversation.run_session_id
      if (sid) await get().openSession(sid, { convNavGen: get().convNavGen })
      return true
    } catch (e) {
      applyActionError(get, set, e)
      return false
    }
  },

  cancelQueue: async () => {
    const id = get().currentConvId
    if (!id) {
      set({ actionError: '没有打开的对话' })
      return false
    }
    try {
      const conversation = await api.cancelQueue(id)
      const conversations = mergeConversation(get().conversations, conversation)
      if (!isIntendedConv(get(), id)) {
        set({ conversations })
        await get().refreshConversations()
        return true
      }
      set({ conversation, conversations, actionError: null })
      await get().refreshConversations()
      return true
    } catch (e) {
      if (isIntendedConv(get(), id)) set({ actionError: errorText(e) })
      return false
    }
  },

  cancelSteer: async () => {
    const id = get().currentConvId
    if (!id) {
      set({ actionError: '没有打开的对话' })
      return false
    }
    try {
      const conversation = await api.cancelSteer(id)
      const conversations = mergeConversation(get().conversations, conversation)
      if (!isIntendedConv(get(), id)) {
        set({ conversations })
        await get().refreshConversations()
        return true
      }
      set({ conversation, conversations, actionError: null })
      await get().refreshConversations()
      return true
    } catch (e) {
      if (isIntendedConv(get(), id)) set({ actionError: errorText(e) })
      return false
    }
  },

  loadTurnChanges: async (sessionId) => {
    const id = get().currentConvId
    const conversation = get().conversation
    const sid =
      sessionId ||
      conversation?.current_session_id ||
      conversation?.run_session_id ||
      conversation?.session_ids?.[conversation.session_ids.length - 1]
    if (!id || !sid) {
      set({ turnChanges: null })
      return
    }
    try {
      const got = await api.getTurnChanges(id, sid)
      if (get().currentConvId === id && isIntendedConv(get(), id)) {
        set({
          turnChanges: {
            sessionId: sid,
            changes: got.changes ?? [],
            editable: !!got.editable,
            reason: got.reason,
          },
        })
      }
    } catch {
      if (get().currentConvId === id && isIntendedConv(get(), id)) set({ turnChanges: { sessionId: sid, changes: [] } })
    }
  },

  applyTurnHunks: async (payload, sessionId) => {
    const id = get().currentConvId
    const sid =
      sessionId ||
      get().turnChanges?.sessionId ||
      get().conversation?.current_session_id ||
      get().conversation?.run_session_id
    if (!id || !sid) {
      set({ actionError: '没有可写回的本轮差异' })
      return false
    }
    try {
      const got = await api.applyTurnHunks(id, sid, payload)
      if (get().currentConvId !== id || !isIntendedConv(get(), id)) return true
      set({
        conversation: got.conversation ?? get().conversation,
        turnChanges: {
          sessionId: sid,
          changes: got.changes ?? [],
          editable: !!got.editable,
        },
        actionError: null,
      })
      return true
    } catch (e) {
      set({ actionError: errorText(e) })
      return false
    }
  },

  rollbackTurn: async (sessionId) => {
    const id = get().currentConvId
    if (!id || !sessionId) {
      set({ actionError: '没有可撤销的轮次' })
      return false
    }
    try {
      const got = await api.rollbackTurn(id, sessionId)
      if (get().currentConvId !== id || !isIntendedConv(get(), id)) return true
      const conversation = got.conversation ?? get().conversation
      set({
        conversation,
        turnChanges: null,
        actionError: null,
      })
      if (conversation) await get().refreshConversations()
      return true
    } catch (e) {
      applyActionError(get, set, e)
      return false
    }
  },


  recoverRollback: async () => {
    const id = get().currentConvId
    if (!id) {
      set({ actionError: '没有打开的对话' })
      return false
    }
    try {
      const got = await api.recoverRollback(id)
      if (get().currentConvId !== id || !isIntendedConv(get(), id)) return true
      const conversation = got.conversation ?? get().conversation
      const pending = conversation?.pending_rollback
      set({
        conversation,
        actionError: pending ? pending.message || got.error || '撤销仍未完成，已保留操作日志和备份' : null,
      })
      if (conversation) await get().refreshConversations()
      return !pending
    } catch (e) {
      applyActionError(get, set, e)
      return false
    }
  },

  resumeConversation: async (apiKey) => {
    const id = get().currentConvId
    if (!id) {
      set({ actionError: '没有打开的对话' })
      return false
    }
    try {
      const conversation = await api.resumeConversation(id, apiKey)
      const conversations = mergeConversation(get().conversations, conversation)
      if (!isIntendedConv(get(), id)) {
        set({ conversations })
        await get().refreshConversations()
        return true
      }
      set({ conversation, conversations, actionError: null })
      await get().refreshConversations()
      if (!isIntendedConv(get(), id)) return true
      const sid = conversation.current_session_id || conversation.run_session_id
      if (sid) await get().openSession(sid, { convNavGen: get().convNavGen })
      return true
    } catch (e) {
      applyActionError(get, set, e)
      return false
    }
  },

  deleteConversation: async (id) => {
    try {
      await api.deleteConversation(id)
      const byConv = { ...get().pendingSelectionsByConv }
      delete byConv[id]
      set({
        pendingSelectionsByConv: byConv,
        pendingSelections: get().currentConvId === id ? [] : selectionsFor(byConv, get().currentConvId),
      })
      if (get().currentConvId === id) {
        await get().openConversation('')
      }
      await get().refreshConversations()
      set({ actionError: null })
      return true
    } catch (e) {
      set({ actionError: errorText(e) })
      return false
    }
  },

  setConversationKey: async (apiKey) => {
    const id = get().currentConvId
    if (!id) {
      set({ actionError: '没有打开的对话' })
      return false
    }
    try {
      const conversation = await api.setConversationKey(id, apiKey)
      const conversations = mergeConversation(get().conversations, conversation)
      if (!isIntendedConv(get(), id)) {
        set({ conversations })
        return true
      }
      set({ conversation, conversations, actionError: null })
      return true
    } catch (e) {
      if (isIntendedConv(get(), id)) set({ actionError: errorText(e) })
      return false
    }
  },

  createSession: async (params) => {
    const convId = get().currentConvId
    const cgen = get().convNavGen
    const res = await api.createSession(params)
    await get().refreshSessions()
    if (!isIntendedConv(get(), convId) || get().convNavGen !== cgen) return res.session_id
    await get().openSession(res.session_id, { convNavGen: cgen })
    return res.session_id
  },

  openSession: async (id, opts) => {
    const cgen = opts?.convNavGen
    const sgen = get().sessionNavGen + 1
    set({ sessionNavGen: sgen })
    const sessionStillCurrent = () => {
      if (get().sessionNavGen !== sgen) return false
      if (cgen != null && get().convNavGen !== cgen) return false
      return true
    }
    if (!id) {
      if (!sessionStillCurrent()) return
      set({ currentId: null, live: null, tree: null, selectedNodeId: null, fileView: null, fileDirty: false, pendingFileNav: null, events: [] })
      return
    }
    if (!sessionStillCurrent()) return
    set({ currentId: id, events: [], live: null, tree: null, selectedNodeId: null, fileView: null, fileDirty: false, pendingFileNav: null })
    get().disconnect()
    try {
      const detail = await api.getSession(id)
      if (!sessionStillCurrent() || get().currentId !== id) return
      // currentId flips before this await, so the SSE can already have
      // delivered events newer than the detail snapshot (e.g. plan_review
      // pending while the detail read still says planning); blindly
      // overwriting would strand the UI in the older phase with no further
      // phase event coming. Reconcile: never regress the phase, and keep an
      // SSE-delivered tree (any tree_snapshot in the just-reset view is for
      // this session and at least as fresh as the snapshot).
      const detailTree = (detail.tree as TaskTree | null) ?? null
      set((s) => {
        let live = detail.live ?? null
        const sseLive = s.live
        if (
          live &&
          sseLive?.session_id === id &&
          phaseRank(sseLive.phase) >= phaseRank(live.phase)
        ) {
          live = sseLive
        } else if (!live && sseLive?.session_id === id) {
          live = sseLive
        }

        let tree = detailTree
        let selectedNodeId = detailTree?.root_id ?? null
        const sseTreeFresh = s.events.some((e) => e.kind === 'tree_snapshot')
        if (sseTreeFresh && s.tree) {
          tree = s.tree
          selectedNodeId = s.selectedNodeId && s.selectedNodeId in s.tree.nodes ? s.selectedNodeId : s.tree.root_id
        }
        return { live, tree, selectedNodeId }
      })
    } catch (e) {
      // A historical (finished, non-live) session has no live object; that
      // is not an error. Only surface real failures.
      if (!sessionStillCurrent()) return
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

  deleteSession: async (id) => {
    try {
      await api.deleteSession(id)
      // If the deleted session was open, drop its view; the SSE client is
      // torn down by openSession('') via disconnect().
      if (get().currentId === id) {
        await get().openSession('')
      }
      await get().refreshSessions()
      set({ actionError: null })
      return true
    } catch (e) {
      set({ actionError: errorText(e) })
      return false
    }
  },

  selectNode: (id) => set({ selectedNodeId: id }),

  openFile: (path, sessionId, opts) => {
    const planned = planFileOpen(
      get().fileView,
      { path, sessionId: sessionId ?? get().currentId, preferDiff: opts?.preferDiff, line: opts?.line },
      get().fileDirty,
    )
    if (planned.kind === 'reveal') {
      set({ fileView: planned.view })
      return
    }
    if (planned.kind === 'confirm') {
      set({ pendingFileNav: planned.view })
      return
    }
    set({ fileView: planned.view, pendingFileNav: null, fileDirty: false })
  },
  closeFile: () => {
    if (get().fileDirty) {
      set({ pendingFileNav: 'close' })
      return
    }
    set({ fileView: null, pendingFileNav: null, fileDirty: false })
  },
  setFileDirty: (dirty) => set({ fileDirty: dirty }),
  confirmFileNav: () => {
    const p = get().pendingFileNav
    if (p === 'close') {
      set({ fileView: null, pendingFileNav: null, fileDirty: false })
      return
    }
    if (p) {
      set({ fileView: p, pendingFileNav: null, fileDirty: false })
      return
    }
    set({ pendingFileNav: null })
  },
  cancelFileNav: () => set({ pendingFileNav: null }),
  attachSelection: (sel, opts) => {
    const id = opts?.conversationId ?? get().currentConvId
    if (!id) return
    const path = sel.path.trim()
    const text = sel.text
    if (!path || !text) return
    const next: CodeSelection = {
      path,
      start_line: Math.max(1, sel.start_line || 1),
      end_line: Math.max(Math.max(1, sel.start_line || 1), sel.end_line || 1),
      text,
      draft: !!sel.draft,
    }
    const list = [...selectionsFor(get().pendingSelectionsByConv, id), next]
    const byConv = { ...get().pendingSelectionsByConv, [id]: list }
    // Bind to the source conversation. If the user has already opened another
    // conversation, keep the current view and its drafts unchanged.
    if (get().currentConvId !== id) {
      set({ pendingSelectionsByConv: byConv })
      return
    }
    set({ pendingSelectionsByConv: byConv, pendingSelections: list })
  },
  removeSelection: (index) => {
    const id = get().currentConvId
    if (!id) return
    const list = selectionsFor(get().pendingSelectionsByConv, id).filter((_, i) => i !== index)
    const byConv = { ...get().pendingSelectionsByConv, [id]: list }
    set({ pendingSelectionsByConv: byConv, pendingSelections: list })
  },
  clearSelections: () => {
    const id = get().currentConvId
    if (!id) {
      set({ pendingSelections: [] })
      return
    }
    const byConv = { ...get().pendingSelectionsByConv }
    delete byConv[id]
    set({ pendingSelectionsByConv: byConv, pendingSelections: [] })
  },

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
          // Replayed history feeds the timeline only: old high-rank phases
          // in the replay window (plan_review/leaf_approval/ask) must not
          // flash over the current state, and a stale replayed snapshot
          // must not replace the tree. The untagged subscribe snapshot and
          // the live tail drive live/tree state.
          if (ev.replay) {
            return { events }
          }
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
          } else if (ev.kind === 'assistant_stream_start') {
            live = withPhase(live, id, live?.phase ?? 'running', {
              live_stream_id: String(ev.stream_id ?? ''),
              live_assistant: '',
            })
          } else if (ev.kind === 'assistant_delta') {
            const delta = String(ev.text ?? '')
            const full = ev.full == null ? null : String(ev.full)
            const same =
              !ev.stream_id || !live?.live_stream_id || String(ev.stream_id) === live.live_stream_id
            live = withPhase(live, id, live?.phase ?? 'running', {
              live_stream_id: String(ev.stream_id ?? live?.live_stream_id ?? ''),
              live_assistant:
                full != null ? full : same ? `${live?.live_assistant ?? ''}${delta}` : delta,
            })
          } else if (ev.kind === 'assistant_stream_end') {
            const full = String(ev.text ?? live?.live_assistant ?? '')
            live = withPhase(live, id, live?.phase ?? 'running', {
              live_stream_id: String(ev.stream_id ?? live?.live_stream_id ?? ''),
              live_assistant: full,
            })
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
          if (
            ev.kind === 'session_end' ||
            ev.kind === 'user_pause' ||
            ev.kind === 'tool_start' ||
            ev.kind === 'tool_call' ||
            ev.kind === 'ask_pending' ||
            ev.kind === 'ask' ||
            ev.kind === 'ask_timeout' ||
            ev.kind === 'leaf_approval'
          ) {
            void get().refreshConversations()
            const convId = get().currentConvId
            const cgen = get().convNavGen
            if (convId && isIntendedConv(get(), convId)) {
              void api.getConversation(convId).then((conversation) => {
                if (get().currentConvId === convId && get().convNavGen === cgen && isIntendedConv(get(), convId)) {
                  set({ conversation })
                  const sid = conversation.current_session_id || conversation.run_session_id
                  if (sid) void get().loadTurnChanges(sid)
                }
              })
            }
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
  decideLeaf: async (leafId, decision, comment, hunks) =>
    withRun(set, get, (id) => api.decideApproval(id, leafId, decision, comment, hunks)),
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

// phaseRank orders phases along the run lifecycle for reconciling the GET
// detail snapshot against SSE-delivered state: a later snapshot must never
// regress a phase the event stream has already reached. Review rounds after
// execution rank above running (adjust/replan revisits plan_review), and
// terminal phases rank highest.
function phaseRank(p: RunPhase): number {
  switch (p) {
    case 'planning':
      return 0
    case 'running':
      return 1
    case 'plan_review':
      return 3
    case 'leaf_approval':
      return 4
    case 'ask':
      return 5
    default: // paused / done / failed
      return 6
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

function conversationFromError(e: unknown): Conversation | undefined {
  if (!(e instanceof ApiError) || !e.body || typeof e.body !== 'object') return undefined
  const conv = e.body.conversation
  if (!conv || typeof conv !== 'object') return undefined
  return conv as Conversation
}

function applyActionError(
  get: () => { currentConvId: string | null; conversation: Conversation | null; desiredConvId?: string | null },
  set: (partial: { actionError: string; conversation?: Conversation | null }) => void,
  e: unknown,
) {
  const conv = conversationFromError(e)
  const cur = get().currentConvId
  if (conv && isIntendedConv(get(), conv.id) && (!cur || conv.id === cur)) {
    set({ actionError: errorText(e), conversation: conv })
    return
  }
  if (isIntendedConv(get(), cur)) {
    set({ actionError: errorText(e) })
    return
  }
  // A stale failure for a conversation the user already left must not
  // replace the current view's error or document.
}

function errorText(e: unknown): string {
  if (e instanceof Error) return e.message
  return String(e)
}
