/** @vitest-environment jsdom */

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { api } from './api/client'
import { storedLastConv, useStore } from './store'
import type { Conversation } from './types'

function conv(id: string): Conversation {
  return {
    id,
    title: id,
    workdir: `/tmp/${id}`,
    created_at: '',
    updated_at: '',
    settings: { isolate: true, git_commit: false, web: false, browser: false, agent_mode: 'direct' },
    messages: [],
    work_log: [],
    session_ids: [`sess-${id}`],
    current_session_id: `sess-${id}`,
    run_phase: 'done',
    has_api_key: true,
    context_note: '',
  }
}

// This repo's jsdom environment ships a localStorage object without working
// methods; the tests run against a Map-backed stub (same as AiEditDialog's).
const store: Record<string, string> = {}

describe('lastConv restore key', () => {
  beforeEach(() => {
    for (const k of Object.keys(store)) delete store[k]
    vi.stubGlobal('localStorage', {
      getItem: (k: string) => (k in store ? store[k] : null),
      setItem: (k: string, v: string) => {
        store[k] = String(v)
      },
      removeItem: (k: string) => {
        delete store[k]
      },
    })
    useStore.setState({
      currentConvId: null,
      conversation: null,
      desiredConvId: undefined,
      convNavGen: 0,
      sessionNavGen: 0,
      currentId: null,
      live: null,
      tree: null,
      fileView: null,
      fileDirty: false,
      pendingFileNav: null,
      pendingSelections: [],
      pendingSelectionsByConv: {},
      events: [],
      selectedNodeId: null,
      apiError: null,
      actionError: null,
    })
    vi.spyOn(api, 'getConversation').mockImplementation(async (id) => conv(id))
    vi.spyOn(api, 'getSession').mockImplementation(async (id) => ({
      session_id: id,
      workdir: '/tmp',
      live: { phase: 'done', approval_mode: 'auto', session_id: id, workdir: '/tmp' },
      tree: null,
    }))
  })

  afterEach(() => {
    vi.restoreAllMocks()
    vi.unstubAllGlobals()
  })

  it('remembers the last opened conversation and clears on close/delete', async () => {
    await useStore.getState().openConversation('a')
    expect(storedLastConv()).toBe('a')

    // An explicit close clears the key.
    await useStore.getState().openConversation('')
    expect(storedLastConv()).toBeNull()

    // Opening again re-remembers; deleting (even a non-open one) clears it.
    await useStore.getState().openConversation('b')
    expect(storedLastConv()).toBe('b')
    vi.spyOn(api, 'deleteConversation').mockResolvedValue({ ok: true, id: 'b', removed: [] })
    await useStore.getState().deleteConversation('b')
    expect(storedLastConv()).toBeNull()
  })
})
