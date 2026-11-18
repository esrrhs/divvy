/** @vitest-environment jsdom */

import { act } from 'react'
import { createRoot, type Root } from 'react-dom/client'
import { afterEach, beforeEach, describe, expect, it } from 'vitest'
import { useStore } from '../store'
import { TopBar } from './TopBar'
import type { PhaseSnapshot, TaskTree } from '../types'

const live: PhaseSnapshot = {
  phase: 'running',
  approval_mode: 'auto',
  session_id: 'sess-a',
  workdir: '/tmp/x',
}

function tree(calls: number, tokens: number): TaskTree {
  const node = (id: string) => ({
    id,
    parent_id: null,
    type: 'LEAF',
    state: 'COMPLETED',
    title: id,
    contract: {},
    dod: {},
    token_usage: { calls, prompt_tokens: tokens, completion_tokens: 0, total_tokens: tokens },
    created_at: '',
    updated_at: '',
  })
  return {
    id: 't',
    root_id: 'n1',
    model_name: 'qwen3.8:27b',
    nodes: { n1: node('n1') },
  } as unknown as TaskTree
}

describe('TopBar live facts', () => {
  let root: Root
  let host: HTMLElement

  beforeEach(() => {
    host = document.createElement('div')
    document.body.appendChild(host)
    root = createRoot(host)
    ;(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true
    useStore.setState({ conversation: null, tree: null })
  })

  afterEach(() => {
    act(() => root.unmount())
    host.remove()
  })

  it('hides the fact badges without a conversation/tree (no dead placeholders)', () => {
    act(() => root.render(<TopBar live={null} sseStatus="open" />))
    expect(host.textContent).not.toContain('model —')
    expect(host.textContent).not.toContain('tokens —')
    expect(host.textContent).not.toContain('$ —')
  })

  it('shows the conversation model and the tree token totals', () => {
    useStore.setState({
      conversation: {
        id: 'c',
        settings: { model: 'qwen3.8:27b', isolate: true, git_commit: false, web: false, browser: false },
      } as never,
      tree: tree(3, 32000),
    })
    act(() => root.render(<TopBar live={live} sseStatus="open" />))
    expect(host.textContent).toContain('qwen3.8:27b')
    expect(host.textContent).toContain('3 次调用')
    expect(host.textContent).toContain('32k tok')
  })

  it('keeps the token badge hidden while no calls have been made', () => {
    useStore.setState({
      conversation: {
        id: 'c',
        settings: { model: 'm', isolate: true, git_commit: false, web: false, browser: false },
      } as never,
      tree: tree(0, 0),
    })
    act(() => root.render(<TopBar live={live} sseStatus="open" />))
    expect(host.textContent).toContain('m')
    expect(host.textContent).not.toContain('0 次调用')
  })
})
