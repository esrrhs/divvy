/** @vitest-environment jsdom */

import { act } from 'react'
import { createRoot, type Root } from 'react-dom/client'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { api } from './api/client'
import { useStore } from './store'
import App from './App'
import type { Conversation, SessionDetail } from './types'

// Unrelated presentation only. Sidebar, App navigation, and the store stay real.
vi.mock('./components/CenterMain', () => ({ CenterMain: () => null }))
vi.mock('./components/RightPanel', () => ({ RightPanel: () => null }))
vi.mock('./components/TopBar', () => ({ TopBar: () => null }))
vi.mock('./components/AskDialog', () => ({ AskDialog: () => null }))
vi.mock('./components/ActionToast', () => ({ ActionToast: () => null }))

function deferred<T>() {
  let resolve!: (value: T) => void
  const promise = new Promise<T>((res) => {
    resolve = res
  })
  return { promise, resolve }
}

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
    has_api_key: true,
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

describe('App open-project navigation', () => {
  let root: Root
  let host: HTMLElement

  beforeEach(() => {
    ;(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true
    host = document.createElement('div')
    document.body.appendChild(host)
    root = createRoot(host)
    vi.spyOn(api, 'getConversation').mockImplementation(async (id) => conv(id))
    vi.spyOn(api, 'getSession').mockImplementation(async (id) => sessionDetail(id))
    vi.spyOn(api, 'listConversations').mockResolvedValue([conv('a'), conv('b')])
    vi.spyOn(api, 'listSessions').mockResolvedValue([])
    vi.spyOn(api, 'getTurnChanges').mockResolvedValue({ session_id: '', changes: [], editable: false })
    useStore.setState({
      currentConvId: null,
      currentId: null,
      conversation: null,
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
      live: null,
      tree: null,
      connect: () => {},
      disconnect: () => {},
    })
  })

  afterEach(() => {
    act(() => {
      root.unmount()
    })
    host.remove()
    vi.restoreAllMocks()
  })

  it('the Open Project button registers nav intent so a late A send cannot reopen A', async () => {
    const post = deferred<Conversation>()
    vi.spyOn(api, 'postMessage').mockReturnValue(post.promise)

    await useStore.getState().openConversation('a')
    expect(useStore.getState().currentConvId).toBe('a')
    const sendingA = useStore.getState().postMessage('ordinary A message')

    await act(async () => {
      root.render(<App />)
    })
    const open = host.querySelector('button[title="打开项目"]') as HTMLButtonElement
    expect(open).toBeTruthy()
    await act(async () => {
      open.click()
    })
    expect(useStore.getState().currentConvId).toBeNull()
    expect(useStore.getState().currentId).toBeNull()
    // Intent must be cleared; otherwise a late A post still thinks A is intended.
    expect(useStore.getState().desiredConvId ?? null).toBeNull()

    await act(async () => {
      post.resolve(conv('a'))
      await sendingA
    })

    expect(useStore.getState().currentConvId).toBeNull()
    expect(useStore.getState().currentId).toBeNull()
    expect(useStore.getState().conversation).toBeNull()
    // A’s successful send is still recorded in the list.
    expect(useStore.getState().conversations.find((c) => c.id === 'a')).toBeTruthy()
  })

  it('Open Project when already idle stays on the project picker', async () => {
    await act(async () => {
      root.render(<App />)
    })
    const open = host.querySelector('button[title="打开项目"]') as HTMLButtonElement
    await act(async () => {
      open.click()
    })
    expect(useStore.getState().currentConvId).toBeNull()
    expect(useStore.getState().currentId).toBeNull()
    expect(useStore.getState().desiredConvId ?? null).toBeNull()
  })
})
