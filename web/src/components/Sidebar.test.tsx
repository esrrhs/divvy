/** @vitest-environment jsdom */

import { act } from 'react'
import { createRoot, type Root } from 'react-dom/client'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { api } from '../api/client'
import { useStore } from '../store'
import { Sidebar } from './Sidebar'
import type { Conversation } from '../types'

function conv(id: string, title: string, workdir: string, phase = 'done'): Conversation {
  return {
    id,
    title,
    workdir,
    created_at: '',
    updated_at: new Date().toISOString(),
    settings: { isolate: true, git_commit: false, web: false, browser: false, agent_mode: 'direct' },
    messages: [],
    work_log: [],
    session_ids: [],
    run_phase: phase,
    has_api_key: true,
    context_note: '',
  }
}

describe('Sidebar conversation search', () => {
  let root: Root
  let host: HTMLElement

  beforeEach(() => {
    host = document.createElement('div')
    document.body.appendChild(host)
    root = createRoot(host)
    ;(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true
    let rows = [
      conv('a', '修登录超时', '/work/auth'),
      conv('b', 'Add 函数改加法', '/tmp/divvy-guitool'),
      conv('c', '文档整理', '/work/docs', 'paused'),
    ]
    vi.spyOn(api, 'listConversations').mockImplementation(async () => rows)
    vi.spyOn(api, 'renameConversation').mockImplementation(async (id, title) => {
      rows = rows.map((c) => (c.id === id ? { ...c, title } : c))
      return rows.find((c) => c.id === id)!
    })
    useStore.setState({
      conversations: [],
      conversationsLoading: false,
      currentConvId: null,
      desiredConvId: undefined,
      convNavGen: 0,
      sessionNavGen: 0,
      fileView: null,
    })
  })

  afterEach(() => {
    act(() => root.unmount())
    host.remove()
    vi.restoreAllMocks()
  })

  function type(query: string) {
    const input = host.querySelector('input') as HTMLInputElement
    const set = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value')!.set!
    act(() => {
      set.call(input, query)
      input.dispatchEvent(new Event('input', { bubbles: true }))
    })
  }

  function visibleTitles(): string[] {
    return [...host.querySelectorAll('.line-clamp-2')].map((el) => el.textContent ?? '')
  }

  it('renders all conversations and filters by title or path, composing with the phase filter', async () => {
    await act(async () => {
      root.render(<Sidebar onNewSession={() => {}} />)
      await Promise.resolve()
    })
    expect(visibleTitles()).toEqual(['修登录超时', 'Add 函数改加法', '文档整理'])

    type('登录')
    expect(visibleTitles()).toEqual(['修登录超时'])

    type('divvy-guitool')
    expect(visibleTitles()).toEqual(['Add 函数改加法'])

    // Case-insensitive path search.
    type('WORK')
    expect(visibleTitles()).toEqual(['修登录超时', '文档整理'])

    // Composes with the paused phase filter.
    type('')
    await act(async () => {
      const pausedChip = [...host.querySelectorAll('button')].find((b) => b.textContent === '已暂停')
      pausedChip?.dispatchEvent(new MouseEvent('click', { bubbles: true }))
    })
    expect(visibleTitles()).toEqual(['文档整理'])

    // No match: honest empty state with the query echoed.
    type('不存在的对话')
    expect(host.textContent).toContain('没有匹配「不存在的对话」的对话')
  })

  it('hides the search box when there are no conversations', async () => {
    vi.spyOn(api, 'listConversations').mockResolvedValue([])
    await act(async () => {
      root.render(<Sidebar onNewSession={() => {}} />)
      await Promise.resolve()
    })
    expect(host.querySelector('input')).toBeNull()
    expect(host.textContent).toContain('还没有对话')
  })

  it('renames a conversation from the hover ✎ button', async () => {
    await act(async () => {
      root.render(<Sidebar onNewSession={() => {}} />)
      await Promise.resolve()
    })
    const clickBtn = (label: string) => {
      const btn = [...host.querySelectorAll('button')].find(
        (b) => (b.textContent || '').trim() === label || b.getAttribute('title') === label,
      )
      if (!btn) throw new Error(`button ${label} missing`)
      act(() => btn.dispatchEvent(new MouseEvent('click', { bubbles: true })))
    }

    // The ✎ is hover-revealed but always clickable in jsdom.
    clickBtn('重命名对话')
    const input = [...host.querySelectorAll('input')].find((i) => i.title === '回车保存，Esc 取消') as HTMLInputElement
    expect(input.value).toBe('修登录超时')

    const set = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value')!.set!
    act(() => {
      set.call(input, '登录修复：超时分支')
      input.dispatchEvent(new Event('input', { bubbles: true }))
    })
    await act(async () => {
      input.closest('form')!.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }))
      await Promise.resolve()
      await Promise.resolve()
    })
    expect(api.renameConversation).toHaveBeenCalledWith('a', '登录修复：超时分支')
    expect(visibleTitles()).toContain('登录修复：超时分支')
  })
})
