import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { api } from './api/client'
import { useStore } from './store'
import type { Conversation, SessionDetail } from './types'

function conv(id: string, sid = `sess-${id}`): Conversation {
  return {
    id,
    title: id,
    workdir: `/tmp/${id}`,
    created_at: '',
    updated_at: '',
    settings: { isolate: true, git_commit: false, web: false, browser: false, agent_mode: 'direct' },
    messages: [],
    work_log: [],
    session_ids: [sid],
    current_session_id: sid,
    run_phase: 'done',
    has_api_key: false,
    context_note: '',
  }
}

function sessionDetail(id: string): SessionDetail {
  return {
    session_id: id,
    workdir: '/tmp',
    live: { phase: 'done', approval_mode: 'auto', session_id: id, workdir: '/tmp' },
    tree: null,
  }
}

// The open file belongs to the project, not the session: a new run starts a
// new session and must not close the file the user is reading. Switching
// conversations still closes it (a different project).
describe('open file survives session switches within a conversation', () => {
  beforeEach(() => {
    vi.spyOn(api, 'getConversation').mockImplementation(async (id) => conv(id))
    vi.spyOn(api, 'getSession').mockImplementation(async (id) => sessionDetail(id))
    vi.spyOn(api, 'listConversations').mockResolvedValue([conv('a'), conv('b')])
    vi.spyOn(api, 'getTurnChanges').mockResolvedValue({ session_id: '', changes: [], editable: false })
    useStore.setState({
      currentConvId: null,
      conversation: null,
      currentId: null,
      desiredConvId: undefined,
      convNavGen: 0,
      sessionNavGen: 0,
      pendingSelections: [],
      pendingSelectionsByConv: {},
      conversations: [],
      fileView: null,
      fileDirty: false,
      pendingFileNav: null,
      viewerSelection: null,
      apiError: null,
      actionError: null,
      live: null,
      tree: null,
    })
  })

  afterEach(() => {
    vi.restoreAllMocks()
  })

  it('openSession keeps the file, its dirty flag, and the live selection', async () => {
    await useStore.getState().openConversation('a')
    useStore.setState({
      fileView: { sessionId: 'sess-a', path: 'math/math.go' },
      fileDirty: true,
      viewerSelection: { path: 'math/math.go', start_line: 3, end_line: 5, text: 'x', draft: true },
    })

    await useStore.getState().openSession('sess-a')
    expect(useStore.getState().fileView?.path).toBe('math/math.go')
    expect(useStore.getState().fileDirty).toBe(true)
    expect(useStore.getState().viewerSelection?.start_line).toBe(3)

    // A different session of the same conversation: same rule.
    await useStore.getState().openSession('sess-a-2')
    expect(useStore.getState().fileView?.path).toBe('math/math.go')
    expect(useStore.getState().viewerSelection?.text).toBe('x')
  })

  it('switching conversations still closes the file', async () => {
    await useStore.getState().openConversation('a')
    useStore.setState({ fileView: { sessionId: 'sess-a', path: 'math/math.go' } })
    await useStore.getState().openConversation('b')
    expect(useStore.getState().fileView).toBeNull()
    expect(useStore.getState().viewerSelection).toBeNull()
  })

  it('closing the composer conversation (openConversation("")) closes the file', async () => {
    await useStore.getState().openConversation('a')
    useStore.setState({ fileView: { sessionId: 'sess-a', path: 'math/math.go' } })
    await useStore.getState().openConversation('')
    expect(useStore.getState().fileView).toBeNull()
    expect(useStore.getState().viewerSelection).toBeNull()
  })
})
