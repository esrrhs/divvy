/** @vitest-environment jsdom */

import { act } from 'react'
import { createRoot, type Root } from 'react-dom/client'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { api } from './api/client'
import { useStore } from './store'
import App from './App'
import type { Conversation } from './types'

// Regression: a conversation created without a message used to come back from
// the server with `messages: null` (Go nil slice). The view typed it as an
// array and read `.length` in an effect dependency — the whole app unmounted
// into a white screen the moment the user opened the fresh conversation.
// Unrelated presentation stays mocked; ConversationView is real.
vi.mock('./components/RightPanel', () => ({ RightPanel: () => null }))
vi.mock('./components/TopBar', () => ({ TopBar: () => null }))
vi.mock('./components/AskDialog', () => ({ AskDialog: () => null }))
vi.mock('./components/ActionToast', () => ({ ActionToast: () => null }))

function convNullMessages(id: string): Conversation {
  return {
    id,
    title: id,
    workdir: `/tmp/${id}`,
    created_at: '',
    updated_at: '',
    settings: { isolate: true, git_commit: false, web: false, browser: false, agent_mode: 'direct' },
    // What the server used to send before the wire contract fix: null slices.
    messages: null as unknown as Conversation['messages'],
    work_log: null as unknown as Conversation['work_log'],
    session_ids: null as unknown as Conversation['session_ids'],
    has_api_key: true,
    context_note: '',
  }
}

describe('App opens a conversation that reports null arrays', () => {
  let root: Root
  let host: HTMLElement

  beforeEach(() => {
    ;(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true
    host = document.createElement('div')
    document.body.appendChild(host)
    root = createRoot(host)
    vi.spyOn(api, 'listConversations').mockResolvedValue([convNullMessages('fresh')])
    vi.spyOn(api, 'listSessions').mockResolvedValue([])
    useStore.setState({
      currentConvId: null,
      currentId: null,
      conversation: null,
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

  it('stays mounted and shows the empty conversation state', async () => {
    // The old server sent messages/work_log/session_ids as null; the store
    // must hold that shape without crashing the view.
    useStore.setState({ conversation: convNullMessages('fresh'), currentConvId: 'fresh' })

    await act(async () => {
      root.render(<App />)
    })

    // The app did not unmount: the conversation view is there with its
    // empty-state hint, not a white screen.
    expect(host.querySelector('#root') ?? host).toBeTruthy()
    expect(host.textContent).toContain('在下方输入需求')
    expect(useStore.getState().conversation?.id).toBe('fresh')
  })

  it('opening the conversation from the list also survives', async () => {
    vi.spyOn(api, 'getConversation').mockImplementation(async (id) => convNullMessages(id))
    await act(async () => {
      root.render(<App />)
    })
    await act(async () => {
      await useStore.getState().openConversation('fresh')
    })
    expect(useStore.getState().currentConvId).toBe('fresh')
    expect(host.textContent).toContain('在下方输入需求')
  })
})
