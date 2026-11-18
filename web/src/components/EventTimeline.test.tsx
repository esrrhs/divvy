/** @vitest-environment jsdom */

import { act } from 'react'
import { createRoot, type Root } from 'react-dom/client'
import { afterEach, beforeEach, describe, expect, it } from 'vitest'
import { EventTimeline } from './EventTimeline'
import type { StreamEvent } from '../types'

describe('EventTimeline tool_call rendering', () => {
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

  function render(events: StreamEvent[]) {
    act(() => root.render(<EventTimeline events={events} />))
  }

  it('shows duration, output size, and a clipped output preview', () => {
    render([
      {
        seq: 7,
        kind: 'tool_call',
        tool: 'run_bash',
        args: '{"command":"go test ./..."}',
        ok: true,
        duration_ms: 480,
        output_len: 1234,
        output_head: 'ok  \tguitool/math 0.303s',
      },
    ])
    expect(host.textContent).toContain('run_bash')
    expect(host.textContent).toContain('480ms')
    expect(host.textContent).toContain('1.2 kB')
    expect(host.textContent).toContain('ok  \tguitool/math 0.303s')
  })

  it('falls back to the legacy markered preview and omits missing metrics', () => {
    render([
      {
        seq: 3,
        kind: 'tool_call',
        tool: 'read_file',
        args: '{"path":"a.go"}',
        ok: true,
        output_preview: 'package main...(+900 chars)',
      },
    ])
    expect(host.textContent).toContain('read_file')
    expect(host.textContent).not.toContain('ms')
    expect(host.textContent).not.toContain('kB')
    expect(host.textContent).toContain('package main...(+900 chars)')
  })

  it('clips long previews to 200 chars', () => {
    render([
      {
        seq: 9,
        kind: 'tool_call',
        tool: 'execute',
        ok: true,
        output_head: 'x'.repeat(400),
      },
    ])
    const pre = host.querySelector('pre')
    expect(pre?.textContent).toBe('x'.repeat(200) + '…')
  })

  it('marks failures and keeps the error in the summary line', () => {
    render([
      {
        seq: 11,
        kind: 'tool_call',
        tool: 'replace_lines',
        ok: false,
        error: 'old_string not found',
        duration_ms: 39,
        output_head: 'ERROR details',
      },
    ])
    expect(host.textContent).toContain('失败：old_string not found')
    expect(host.textContent).toContain('39ms')
    expect(host.textContent).toContain('ERROR details')
  })

  it('renders no preview block for non-tool events', () => {
    render([{ seq: 2, kind: 'state_change', from: 'PENDING', to: 'RUNNING' }])
    expect(host.querySelector('pre')).toBeNull()
    expect(host.textContent).toContain('PENDING → RUNNING')
  })
})
