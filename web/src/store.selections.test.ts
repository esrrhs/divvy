import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { api } from './api/client'
import { useStore } from './store'
import type { CodeSelection, Conversation } from './types'

function conv(id: string): Conversation {
  return {
    id,
    title: id,
    workdir: `/tmp/${id}`,
    created_at: '',
    updated_at: '',
    settings: { isolate: true, git_commit: false, web: false, browser: false },
    messages: [],
    work_log: [],
    session_ids: [],
    has_api_key: false,
    context_note: '',
  }
}

const sel = (text: string, path = 'pkg/a.go'): CodeSelection => ({
  path,
  start_line: 1,
  end_line: 1,
  text,
  draft: false,
})

describe('pendingSelections isolation', () => {
  const posted: { id: string; selections: CodeSelection[] | undefined }[] = []

  beforeEach(() => {
    posted.length = 0
    vi.spyOn(api, 'getConversation').mockImplementation(async (id) => conv(id))
    vi.spyOn(api, 'listConversations').mockResolvedValue([])
    vi.spyOn(api, 'postMessage').mockImplementation(async (id, _text, opts) => {
      posted.push({ id, selections: opts?.selections })
      return conv(id)
    })
    useStore.setState({
      currentConvId: null,
      conversation: null,
      currentId: null,
      desiredConvId: undefined,
      convNavGen: 0,
      sessionNavGen: 0,
      pendingSelections: [],
      pendingSelectionsByConv: {},
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

  it('keeps citations per conversation and only sends the current conversation’s list', async () => {
    await useStore.getState().openConversation('conv-a')
    useStore.getState().attachSelection(sel('from A'))
    expect(useStore.getState().currentConvId).toBe('conv-a')
    expect(useStore.getState().pendingSelections.map((s) => s.text)).toEqual(['from A'])

    await useStore.getState().openConversation('conv-b')
    expect(useStore.getState().currentConvId).toBe('conv-b')
    expect(useStore.getState().pendingSelections).toEqual([])

    useStore.getState().attachSelection(sel('from B', 'pkg/b.go'))
    expect(useStore.getState().pendingSelections.map((s) => s.text)).toEqual(['from B'])

    const sentB = await useStore.getState().postMessage('hello B')
    expect(sentB).toBe(true)
    expect(posted).toHaveLength(1)
    expect(posted[0]).toEqual({
      id: 'conv-b',
      selections: [sel('from B', 'pkg/b.go')],
    })
    expect(useStore.getState().pendingSelections).toEqual([])

    await useStore.getState().openConversation('conv-a')
    expect(useStore.getState().pendingSelections.map((s) => s.text)).toEqual(['from A'])

    const sentA = await useStore.getState().postMessage('hello A')
    expect(sentA).toBe(true)
    expect(posted[1]).toEqual({
      id: 'conv-a',
      selections: [sel('from A')],
    })
    expect(useStore.getState().pendingSelections).toEqual([])
  })

  it('does not attach a citation when no conversation is open', () => {
    useStore.getState().attachSelection(sel('orphan'))
    expect(useStore.getState().pendingSelections).toEqual([])
  })
})
