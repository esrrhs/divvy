import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { api } from './api/client'
import { useStore } from './store'
import type { CodeSelection, Conversation, SessionDetail } from './types'

function deferred<T>() {
  let resolve!: (value: T) => void
  const promise = new Promise<T>((res) => {
    resolve = res
  })
  return { promise, resolve }
}

function conv(id: string, sid?: string, extra?: Partial<Conversation>): Conversation {
  return {
    id,
    title: extra?.title ?? id,
    workdir: `/tmp/${id}`,
    created_at: '',
    updated_at: '',
    settings: { isolate: true, git_commit: false, web: false, browser: false },
    messages: extra?.messages ?? [],
    work_log: [],
    session_ids: sid ? [sid] : [],
    current_session_id: sid,
    has_api_key: false,
    context_note: '',
    ...extra,
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

const sel = (text: string, path = 'pkg/a.go'): CodeSelection => ({
  path,
  start_line: 1,
  end_line: 1,
  text,
  draft: false,
})

function expectView(convId: string, sessionId: string) {
  const s = useStore.getState()
  expect(s.currentConvId).toBe(convId)
  expect(s.currentId).toBe(sessionId)
  expect(s.conversation?.id).toBe(convId)
}

describe('conversation/session identity under delayed responses', () => {
  beforeEach(() => {
    vi.spyOn(api, 'getConversation').mockImplementation(async (id) => {
      const sid = id === 'conv-a' ? 'sess-a' : id === 'conv-b' ? 'sess-b' : `sess-${id}`
      return conv(id, sid)
    })
    vi.spyOn(api, 'getSession').mockImplementation(async (id) => sessionDetail(id))
    vi.spyOn(api, 'listConversations').mockResolvedValue([])
    useStore.setState({
      currentConvId: null,
      conversation: null,
      currentId: null,
      live: null,
      tree: null,
      desiredConvId: undefined,
      convNavGen: 0,
      sessionNavGen: 0,
      pendingSelections: [],
      pendingSelectionsByConv: {},
      conversations: [],
      fileView: null,
      fileDirty: false,
      pendingFileNav: null,
      apiError: null,
      actionError: null,
    })
  })

  afterEach(() => {
    vi.restoreAllMocks()
  })

  it('does not let a slower earlier openConversation replace the later chosen conversation or its session', async () => {
    const a = deferred<Conversation>()
    vi.mocked(api.getConversation).mockImplementation(async (id) => {
      if (id === 'conv-a') return a.promise
      return conv(id, 'sess-b')
    })

    const openingA = useStore.getState().openConversation('conv-a')
    await useStore.getState().openConversation('conv-b')
    expectView('conv-b', 'sess-b')

    a.resolve(conv('conv-a', 'sess-a'))
    await openingA
    await Promise.resolve()
    await Promise.resolve()

    expectView('conv-b', 'sess-b')
    expect(useStore.getState().live?.session_id).toBe('sess-b')
    expect(vi.mocked(api.getSession).mock.calls.map((c) => c[0])).not.toContain('sess-a')
  })

  it('ignores a late getSession from A after B is already open', async () => {
    const aConv = deferred<Conversation>()
    const aDetail = deferred<SessionDetail>()
    vi.mocked(api.getConversation).mockImplementation(async (id) => {
      if (id === 'conv-a') return aConv.promise
      return conv(id, 'sess-b')
    })
    vi.mocked(api.getSession).mockImplementation(async (id) => {
      if (id === 'sess-a') return aDetail.promise
      return sessionDetail(id)
    })

    const openingA = useStore.getState().openConversation('conv-a')
    aConv.resolve(conv('conv-a', 'sess-a'))
    // Wait for A to be applied and its session request to start, before opening B.
    for (let i = 0; i < 20 && useStore.getState().currentConvId !== 'conv-a'; i++) {
      await Promise.resolve()
    }
    expect(useStore.getState().currentConvId).toBe('conv-a')
    expect(useStore.getState().currentId).toBe('sess-a')

    await useStore.getState().openConversation('conv-b')
    expectView('conv-b', 'sess-b')

    aDetail.resolve({
      session_id: 'sess-a',
      workdir: '/tmp/a',
      live: { phase: 'running', approval_mode: 'auto', session_id: 'sess-a', workdir: '/tmp/a' },
      tree: null,
    })
    await openingA
    await Promise.resolve()
    await Promise.resolve()

    expectView('conv-b', 'sess-b')
    expect(useStore.getState().live?.session_id).toBe('sess-b')
    expect(useStore.getState().live?.phase).not.toBe('running')
  })

  it('keeps B selected when a late A postMessage returns, updates A’s record, and does not clear B’s drafts', async () => {
    const post = deferred<Conversation>()
    const list = deferred<Conversation[]>()
    vi.spyOn(api, 'postMessage').mockReturnValue(post.promise)
    vi.mocked(api.listConversations).mockReturnValue(list.promise)

    await useStore.getState().openConversation('conv-a')
    expectView('conv-a', 'sess-a')
    useStore.getState().attachSelection(sel('draft A'))

    const sendingA = useStore.getState().postMessage('ordinary A message')
    await useStore.getState().openConversation('conv-b')
    expectView('conv-b', 'sess-b')
    useStore.getState().attachSelection(sel('draft B', 'pkg/b.go'))
    expect(useStore.getState().pendingSelections.map((s) => s.text)).toEqual(['draft B'])

    const updatedA = conv('conv-a', 'sess-a', {
      title: 'A after send',
      messages: [{ id: 'm1', role: 'user', kind: 'user', text: 'ordinary A message', created_at: '' }],
    })
    post.resolve(updatedA)
    await Promise.resolve()
    await Promise.resolve()
    await Promise.resolve()

    expectView('conv-b', 'sess-b')
    expect(useStore.getState().pendingSelections.map((s) => s.text)).toEqual(['draft B'])
    expect(useStore.getState().pendingSelectionsByConv['conv-a'] ?? []).toEqual([])
    const listedA = useStore.getState().conversations.find((c) => c.id === 'conv-a')
    expect(listedA?.title).toBe('A after send')
    expect(listedA?.messages?.[0]?.text).toBe('ordinary A message')

    list.resolve([updatedA, conv('conv-b', 'sess-b')])
    await sendingA
    await Promise.resolve()
    await Promise.resolve()

    expectView('conv-b', 'sess-b')
    expect(useStore.getState().pendingSelections.map((s) => s.text)).toEqual(['draft B'])
    expect(useStore.getState().pendingSelectionsByConv['conv-a'] ?? []).toEqual([])
    expect(useStore.getState().conversation?.id).toBe('conv-b')
  })

  it("late A postMessage cannot restore A after openConversation('')", async () => {
    const post = deferred<Conversation>()
    vi.spyOn(api, 'postMessage').mockReturnValue(post.promise)

    await useStore.getState().openConversation('conv-a')
    expectView('conv-a', 'sess-a')
    const sendingA = useStore.getState().postMessage('ordinary A message')
    await useStore.getState().openConversation('')
    expect(useStore.getState().currentConvId).toBeNull()
    expect(useStore.getState().currentId).toBeNull()
    expect(useStore.getState().desiredConvId).toBeNull()

    post.resolve(conv('conv-a', 'sess-a', { title: 'A after send' }))
    await sendingA
    await Promise.resolve()
    await Promise.resolve()

    expect(useStore.getState().currentConvId).toBeNull()
    expect(useStore.getState().currentId).toBeNull()
    expect(useStore.getState().conversation).toBeNull()
    expect(useStore.getState().conversations.find((c) => c.id === 'conv-a')?.title).toBe('A after send')
  })

  it('a successful send only removes the citations that were sent, not drafts added while it was in flight', async () => {
    const post = deferred<Conversation>()
    vi.spyOn(api, 'postMessage').mockReturnValue(post.promise)

    await useStore.getState().openConversation('conv-a')
    useStore.getState().attachSelection(sel('sent A'))
    const sending = useStore.getState().postMessage('ordinary A message')
    useStore.getState().attachSelection(sel('added during send', 'pkg/new.go'))
    expect(useStore.getState().pendingSelections.map((s) => s.text)).toEqual(['sent A', 'added during send'])

    post.resolve(conv('conv-a', 'sess-a'))
    await sending
    await Promise.resolve()
    await Promise.resolve()

    expectView('conv-a', 'sess-a')
    expect(useStore.getState().pendingSelections.map((s) => s.text)).toEqual(['added during send'])
  })

  it('deleting the current conversation leaves the picker and ignores a late A post', async () => {
    const post = deferred<Conversation>()
    vi.spyOn(api, 'postMessage').mockReturnValue(post.promise)
    vi.spyOn(api, 'deleteConversation').mockResolvedValue(undefined as never)

    await useStore.getState().openConversation('conv-a')
    const sendingA = useStore.getState().postMessage('ordinary A message')
    const ok = await useStore.getState().deleteConversation('conv-a')
    expect(ok).toBe(true)
    expect(useStore.getState().currentConvId).toBeNull()
    expect(useStore.getState().desiredConvId).toBeNull()

    post.resolve(conv('conv-a', 'sess-a'))
    await sendingA
    await Promise.resolve()
    await Promise.resolve()
    expect(useStore.getState().currentConvId).toBeNull()
    expect(useStore.getState().currentId).toBeNull()
  })
})
