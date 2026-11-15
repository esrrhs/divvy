/** @vitest-environment jsdom */

import { act } from 'react'
import { createRoot, type Root } from 'react-dom/client'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { api } from '../api/client'
import { useStore } from '../store'
import type { AiEditApplyRequest, AiEditProposal, AiEditResponse } from '../types'
import { AiEditDialog } from './AiEditDialog'

const p = (path: string, text: string): AiEditProposal => ({
  path,
  base_hash: `hash-${path}`,
  start_line: 2,
  end_line: 2,
  old_text: 'old\n',
  new_text: text,
  hunks: [
    {
      index: 0,
      lines: [
        { type: 'remove', old_no: 2, text: 'old' },
        { type: 'add', new_no: 2, text: text.trim() },
      ],
    },
  ],
  preview: `@@ 块 1 @@\n+${text}`,
  context_before: 'package p\n\n',
  context_after: '}\n',
})

const RESULT: AiEditResponse = {
  ...p('pkg/a.go', 'A2\n'),
  proposals: [p('pkg/a.go', 'A2\n'), p('pkg/b.go', 'B2\n')],
}

function typeIntoImpl(el: HTMLTextAreaElement, text: string) {
  const native = Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, 'value')
  native?.set?.call(el, text)
  el.dispatchEvent(new Event('input', { bubbles: true }))
}

describe('AiEditDialog multi-file review', () => {
  let root: Root
  let host: HTMLElement

  beforeEach(() => {
    host = document.createElement('div')
    document.body.appendChild(host)
    root = createRoot(host)
    ;(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true
    useStore.setState({
      currentConvId: 'conv-a',
      actionError: null,
      fileReloadNonce: 0,
      aiEditOpen: true,
      aiEditPath: 'pkg/a.go',
      aiEditSelection: { start_line: 2, end_line: 2 },
      aiEditTargets: [
        { path: 'pkg/a.go', selection: { start_line: 2, end_line: 2 } },
        { path: 'pkg/b.go', selection: { start_line: 2, end_line: 2 } },
      ],
      aiEditResult: null,
      aiEditStreaming: '',
      aiEditProgress: '',
      aiEditBusy: false,
      aiEditApplied: null,
      aiEditHistory: [],
      aiEditUndoDepth: 0,
      fileView: null,
      fileDirty: false,
    })
  })

  afterEach(() => {
    act(() => root.unmount())
    host.remove()
    vi.restoreAllMocks()
  })

  async function render() {
    await act(async () => {
      root.render(<AiEditDialog />)
      await Promise.resolve()
    })
  }

  function button(label: string): HTMLButtonElement {
    const b = Array.from(host.querySelectorAll('button')).find((x) => x.textContent?.includes(label))
    if (!b) throw new Error(`button not found: ${label}`)
    return b as HTMLButtonElement
  }

  it('shows one chip per file and switches the diff without mixing decisions', async () => {
    vi.spyOn(api, 'aiEditStream').mockResolvedValue(RESULT)
    const apply = vi.spyOn(api, 'aiEditApply').mockResolvedValue({
      path: 'pkg/a.go',
      applied: 2,
      rejected: 0,
      new_content: 'A2\n',
      conflict: false,
      paths: ['pkg/a.go', 'pkg/b.go'],
    })

    await render()
    await act(async () => {
      typeIntoImpl(host.querySelector('textarea') as HTMLTextAreaElement, '一起改')
      await Promise.resolve()
    })
    await act(async () => {
      button('生成候选').click()
      await Promise.resolve()
    })

    expect(host.textContent).toContain('2 个文件')
    expect(host.textContent).toContain('a.go')
    expect(host.textContent).toContain('b.go')
    // The first file's diff is shown by default.
    expect(host.textContent).toContain('A2')

    // Reject the only hunk of a.go, then switch to b.go: b.go must still be
    // accepted (decisions are per file, not per hunk index).
    await act(async () => {
      button('接受中（点击拒绝）').click()
      await Promise.resolve()
    })
    await act(async () => {
      const chip = Array.from(host.querySelectorAll('button')).find((b) => b.textContent?.includes('b.go:'))
      expect(chip).toBeTruthy()
      ;(chip as HTMLButtonElement).click()
      await Promise.resolve()
    })
    expect(host.textContent).toContain('B2')
    expect(host.textContent).toContain('将应用 1 / 1 块')

    await act(async () => {
      Array.from(host.querySelectorAll('button')).find((b) => b.textContent?.includes('应用 1 块'))?.click()
      await Promise.resolve()
    })
    expect(apply).toHaveBeenCalledTimes(1)
    const body = apply.mock.calls[0][1] as AiEditApplyRequest
    expect(body.proposals?.map((x) => x.path)).toEqual(['pkg/a.go', 'pkg/b.go'])
    expect(body.proposals?.[0].decisions).toEqual([false])
    expect(body.proposals?.[1].decisions).toEqual([true])
    expect(useStore.getState().aiEditApplied).toEqual({ applied: 2, rejected: 0, paths: ['pkg/a.go', 'pkg/b.go'] })
  })

  it('surfaces a failed file without hiding the successful ones', async () => {
    vi.spyOn(api, 'aiEditStream').mockResolvedValue({
      ...RESULT,
      proposals: [p('pkg/a.go', 'A2\n'), { ...p('pkg/b.go', 'old\n'), error: 'LLM 调用失败: 503' }],
    })
    await render()
    await act(async () => {
      typeIntoImpl(host.querySelector('textarea') as HTMLTextAreaElement, '一起改')
      await Promise.resolve()
    })
    await act(async () => {
      button('生成候选').click()
      await Promise.resolve()
    })
    expect(host.textContent).toContain('失败')
    await act(async () => {
      const chip = Array.from(host.querySelectorAll('button')).find((b) => b.textContent?.includes('b.go:'))
      ;(chip as HTMLButtonElement).click()
      await Promise.resolve()
    })
    expect(host.textContent).toContain('生成失败')
    expect(host.textContent).toContain('这个文件不会被写回')
  })

  it('removing a file from the round drops the stale proposals', async () => {
    vi.spyOn(api, 'aiEditStream').mockResolvedValue(RESULT)
    await render()
    await act(async () => {
      typeIntoImpl(host.querySelector('textarea') as HTMLTextAreaElement, '一起改')
      await Promise.resolve()
    })
    await act(async () => {
      button('生成候选').click()
      await Promise.resolve()
    })
    expect(useStore.getState().aiEditResult).not.toBeNull()
    const remove = Array.from(host.querySelectorAll('button')).find((b) => b.textContent === '×')
    expect(remove).toBeTruthy()
    await act(async () => {
      ;(remove as HTMLButtonElement).click()
      await Promise.resolve()
    })
    expect(useStore.getState().aiEditTargets).toHaveLength(1)
    expect(useStore.getState().aiEditResult).toBeNull()
  })
})
