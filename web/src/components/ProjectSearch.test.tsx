/** @vitest-environment jsdom */

import { act } from 'react'
import { createRoot, type Root } from 'react-dom/client'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { api } from '../api/client'
import { useStore } from '../store'
import type { ProjectSearchResponse } from '../types'
import { ProjectSearch } from './ProjectSearch'

function deferred<T>() {
  let resolve!: (value: T) => void
  let reject!: (reason?: unknown) => void
  const promise = new Promise<T>((res, rej) => {
    resolve = res
    reject = rej
  })
  return { promise, resolve, reject }
}

function resp(hits: ProjectSearchResponse['hits'], extra?: Partial<ProjectSearchResponse>): ProjectSearchResponse {
  return {
    query: 'token',
    literal: true,
    regex: false,
    case_sensitive: false,
    hits,
    truncated: false,
    scanned: hits.length,
    ...extra,
  }
}

describe('ProjectSearch scope isolation', () => {
  let root: Root
  let host: HTMLElement
  const searches: { conversation?: string; session?: string; q: string }[] = []
  const pending = new Map<string, ReturnType<typeof deferred<ProjectSearchResponse>>>()

  beforeEach(() => {
    searches.length = 0
    pending.clear()
    vi.useFakeTimers()
    ;(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true
    host = document.createElement('div')
    document.body.appendChild(host)
    root = createRoot(host)
    useStore.setState({
      currentConvId: 'conv-a',
      currentId: 'sess-a',
      conversation: null,
      desiredConvId: undefined,
      convNavGen: 0,
      sessionNavGen: 0,
      pendingSelections: [],
      pendingSelectionsByConv: {},
      fileView: null,
      fileDirty: false,
      pendingFileNav: null,
    })
    vi.spyOn(api, 'fsSearch').mockImplementation(async (q, opts) => {
      searches.push({ conversation: opts?.conversation, session: opts?.session, q })
      const key = opts?.conversation ?? opts?.session ?? 'none'
      const d = deferred<ProjectSearchResponse>()
      pending.set(key, d)
      return d.promise
    })
    vi.spyOn(api, 'fsFile').mockImplementation(async (path) => ({
      path,
      content: `line 1\nline 2\nline 3\ncurrent disk line for ${path}\nline 5\n`,
      size: 80,
      hash: 'abc',
      editable: true,
      mode: 0o644,
    }))
  })

  afterEach(() => {
    act(() => {
      root.unmount()
    })
    host.remove()
    vi.useRealTimers()
    vi.restoreAllMocks()
  })

  async function renderSearch() {
    await act(async () => {
      root.render(<ProjectSearch />)
    })
  }

  async function typeQuery(text: string) {
    const input = host.querySelector('input[placeholder="搜索项目文件内容"]') as HTMLInputElement
    expect(input).toBeTruthy()
    await act(async () => {
      const native = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value')
      native?.set?.call(input, text)
      input.dispatchEvent(new Event('input', { bubbles: true }))
    })
  }

  function citeButtons() {
    return Array.from(host.querySelectorAll('button')).filter((b) => b.textContent === '引用到对话')
  }

  it('clears previous hits on project switch and ignores a late response from the old project', async () => {
    await renderSearch()
    await typeQuery('alpha')
    await act(async () => {
      await vi.advanceTimersByTimeAsync(300)
    })
    expect(searches).toEqual([{ conversation: 'conv-a', session: 'sess-a', q: 'alpha' }])

    const lateA = pending.get('conv-a')
    expect(lateA).toBeTruthy()

    await act(async () => {
      useStore.setState({ currentConvId: 'conv-b', currentId: 'sess-b' })
    })
    expect(host.textContent).not.toMatch(/pkg\/a\.go/)
    expect(citeButtons()).toHaveLength(0)

    await act(async () => {
      lateA!.resolve(resp([{ path: 'pkg/a.go', line: 2, preview: 'from project A' }]))
      await Promise.resolve()
    })
    expect(host.textContent).not.toMatch(/pkg\/a\.go/)
    expect(host.textContent).not.toMatch(/from project A/)

    await act(async () => {
      await vi.advanceTimersByTimeAsync(300)
    })
    expect(searches[1]).toEqual({ conversation: 'conv-b', session: 'sess-b', q: 'alpha' })

    await act(async () => {
      pending.get('conv-b')!.resolve(resp([{ path: 'pkg/b.go', line: 4, preview: 'current disk line for pkg/b.go' }]))
      await Promise.resolve()
    })
    expect(host.textContent).toMatch(/pkg\/b\.go:L4/)
    expect(host.textContent).not.toMatch(/pkg\/a\.go/)

    const cite = citeButtons()[0]
    expect(cite).toBeTruthy()
    await act(async () => {
      cite.click()
      await Promise.resolve()
      await Promise.resolve()
      await Promise.resolve()
    })
    expect(api.fsFile).toHaveBeenCalledWith('pkg/b.go', 'sess-b', 'conv-b')
    expect(useStore.getState().pendingSelections).toEqual([
      {
        path: 'pkg/b.go',
        start_line: 4,
        end_line: 4,
        text: 'current disk line for pkg/b.go',
        draft: false,
      },
    ])
    expect(host.textContent).toMatch(/当前源行/)
  })

  it('does not refill results from a late request after the query is cleared', async () => {
    await renderSearch()
    await typeQuery('alpha')
    await act(async () => {
      await vi.advanceTimersByTimeAsync(300)
    })
    const first = pending.get('conv-a')
    await typeQuery('')
    await act(async () => {
      first!.resolve(resp([{ path: 'pkg/a.go', line: 1, preview: 'stale' }]))
      await Promise.resolve()
    })
    expect(host.textContent).not.toMatch(/pkg\/a\.go/)
    expect(host.textContent).not.toMatch(/stale/)
  })

  it('discards a delayed citation after the search panel unmounts instead of attaching it to the new conversation', async () => {
    vi.spyOn(api, 'getConversation').mockImplementation(async (id) => ({
      id,
      title: id,
      workdir: `/tmp/${id}`,
      created_at: '',
      updated_at: '',
      settings: { isolate: true, git_commit: false, web: false, browser: false },
      messages: [],
      work_log: [],
      session_ids: [],
      current_session_id: id === 'conv-b' ? 'sess-b' : 'sess-a',
      has_api_key: false,
      context_note: '',
    }))
    vi.spyOn(api, 'getSession').mockImplementation(async (id) => ({
      session_id: id,
      workdir: '/tmp',
      live: { phase: 'done', approval_mode: 'auto', session_id: id, workdir: '/tmp' },
      tree: null,
    }))
    vi.spyOn(api, 'listConversations').mockResolvedValue([])

    const file = deferred<any>()
    ;(api.fsSearch as any).mockResolvedValue(
      resp([{ path: 'a.ts', line: 1, preview: 'from A' }]),
    )
    ;(api.fsFile as any).mockReturnValue(file.promise)

    await renderSearch()
    await typeQuery('alpha')
    await act(async () => {
      await vi.advanceTimersByTimeAsync(300)
    })
    const cite = citeButtons()[0]
    expect(cite).toBeTruthy()
    await act(async () => {
      cite.click()
    })
    expect(api.fsFile).toHaveBeenCalledWith('a.ts', 'sess-a', 'conv-a')

    // Switching the sidebar from Search to Conversations unmounts ProjectSearch.
    await act(async () => {
      root.unmount()
    })
    host.remove()
    host = document.createElement('div')
    document.body.appendChild(host)
    root = createRoot(host)

    await useStore.getState().openConversation('conv-b')
    expect(useStore.getState().currentConvId).toBe('conv-b')
    expect(useStore.getState().currentId).toBe('sess-b')

    await act(async () => {
      file.resolve({ path: 'a.ts', content: 'from A\n', size: 7, hash: 'x', editable: true, mode: 0o644 })
      await Promise.resolve()
      await Promise.resolve()
      await Promise.resolve()
    })

    const state = useStore.getState()
    expect(state.currentConvId).toBe('conv-b')
    expect(state.currentId).toBe('sess-b')
    expect(state.pendingSelections).toEqual([])
    expect(state.pendingSelectionsByConv['conv-b'] ?? []).toEqual([])
    expect(state.pendingSelectionsByConv['conv-a'] ?? []).toEqual([])
  })
})
