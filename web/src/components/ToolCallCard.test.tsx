/** @vitest-environment jsdom */

import { act } from 'react'
import { createRoot, type Root } from 'react-dom/client'
import { afterEach, beforeEach, describe, expect, it } from 'vitest'
import { formatDuration, formatOutputLen, ToolCallCard } from './ToolCallCard'
import type { ToolActivity } from '../types'

describe('formatDuration / formatOutputLen', () => {
  it('formats compact durations and sizes', () => {
    expect(formatDuration(580)).toBe('580ms')
    expect(formatDuration(1500)).toBe('1.5s')
    expect(formatDuration(12000)).toBe('12s')
    expect(formatDuration(undefined)).toBe('')
    expect(formatDuration(Number.NaN)).toBe('')
    expect(formatOutputLen(512)).toBe('512 B')
    expect(formatOutputLen(2048)).toBe('2.0 kB')
    expect(formatOutputLen(0)).toBe('')
  })
})

describe('ToolCallCard', () => {
  let root: Root
  let host: HTMLElement

  beforeEach(() => {
    host = document.createElement('div')
    document.body.appendChild(host)
    root = createRoot(host)
    ;(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true
  })

  afterEach(() => {
    act(() => root.unmount())
    host.remove()
  })

  function render(tool: ToolActivity) {
    act(() => root.render(<ToolCallCard tool={tool} />))
  }

  function toggle() {
    const btn = host.querySelector('button')
    if (!btn) throw new Error('header button missing')
    act(() => btn.dispatchEvent(new MouseEvent('click', { bubbles: true })))
  }

  it('renders a collapsed status row; expanding reveals args and output', () => {
    render({
      tool: 'execute',
      ok: true,
      args: '{"command":"go test ./..."}',
      output_head: 'ok  \tpkg/addtest',
      output_len: 19,
      duration_ms: 580,
    })
    expect(host.textContent).toContain('execute')
    expect(host.textContent).toContain('580ms')
    // Collapsed: no args or output body yet.
    expect(host.textContent).not.toContain('go test ./...')

    toggle()
    expect(host.textContent).toContain('参数')
    expect(host.textContent).toContain('go test ./...')
    expect(host.textContent).toContain('输出')
    expect(host.textContent).toContain('pkg/addtest')
    // A whole-recorded output shows no truncation gap.
    expect(host.textContent).not.toContain('已省略')
  })

  it('shows the tail and a gap separator only when the middle was dropped', () => {
    const head = 'h'.repeat(600)
    const tail = 'ok\ntail line' // 12 chars
    render({
      tool: 'execute',
      ok: true,
      output_head: head,
      output_tail: tail,
      output_len: 600 + 1400 + 12,
      duration_ms: 12000,
    })
    toggle()
    expect(host.textContent).toContain('tail line')
    expect(host.textContent).toContain('中间 1400 字符已省略')
    expect(host.textContent).toContain('12s')
  })

  it('marks failures and surfaces the error text', () => {
    render({
      tool: 'execute',
      ok: false,
      error: 'exit status 1: build failed',
      duration_ms: 1500,
    })
    expect(host.textContent).toContain('失败')
    // The error text surfaces on expand.
    toggle()
    expect(host.textContent).toContain('exit status 1: build failed')
    expect((host.firstElementChild as HTMLElement).className).toContain('border-rose-900')
  })
})
