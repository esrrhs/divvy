/** @vitest-environment jsdom */

import { act } from 'react'
import { createRoot, type Root } from 'react-dom/client'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { api } from '../api/client'
import { useStore } from '../store'
import type { AiEditRequest, AiEditResponse } from '../types'
import { AiEditDialog } from './AiEditDialog'

const RESULT: AiEditResponse = {
  path: 'pkg/a.go',
  base_hash: 'hash-1',
  start_line: 2,
  end_line: 3,
  old_text: 'func A() int {\n\treturn 1\n}\n',
  new_text: 'func A() int {\n\treturn 2\n}\n',
  hunks: [
    {
      index: 0,
      lines: [
        { type: 'context', old_no: 2, new_no: 2, text: 'func A() int {' },
        { type: 'remove', old_no: 3, text: '\treturn 1' },
        { type: 'add', new_no: 3, text: '\treturn 2' },
        { type: 'context', old_no: 4, new_no: 4, text: '}' },
      ],
    },
  ],
  preview: '@@ 块 1 @@\n-\treturn 1\n+\treturn 2\n',
}

function typeInto(el: HTMLTextAreaElement, text: string) {
  const native = Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, 'value')
  native?.set?.call(el, text)
  el.dispatchEvent(new Event('input', { bubbles: true }))
}

describe('AiEditDialog', () => {
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
      aiEditSelection: { start_line: 2, end_line: 3 },
      aiEditResult: null,
      aiEditStreaming: '',
      aiEditBusy: false,
      aiEditApplied: null,
      aiEditHistory: [],
      aiEditUndoDepth: 0,
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

  it('streams model output, shows the diff, and applies accepted hunks', async () => {
    let sawDelta = ''
    vi.spyOn(api, 'aiEditStream').mockImplementation(async (_id, _body, onDelta) => {
      onDelta('func A() int {\n')
      onDelta('\treturn 2\n}\n')
      sawDelta = 'done'
      return RESULT
    })
    const apply = vi.spyOn(api, 'aiEditApply').mockResolvedValue({
      path: 'pkg/a.go',
      applied: 1,
      rejected: 0,
      new_content: 'package p\n\nfunc A() int {\n\treturn 2\n}\n',
      conflict: false,
    })

    await render()
    const ta = host.querySelector('textarea') as HTMLTextAreaElement
    expect(ta).toBeTruthy()

    await act(async () => {
      typeInto(ta, '把返回值改成 2')
      await Promise.resolve()
    })

    const generate = Array.from(host.querySelectorAll('button')).find((b) => b.textContent?.includes('生成候选'))
    expect(generate).toBeTruthy()
    await act(async () => {
      ;(generate as HTMLButtonElement).click()
      await Promise.resolve()
    })

    expect(sawDelta).toBe('done')
    expect(api.aiEditStream).toHaveBeenCalledTimes(1)
    // The diff is rendered from hunks, not from raw preview text.
    expect(host.textContent).toContain('@@ 块 1 @@')
    expect(host.textContent).toContain('return 2')
    expect(host.textContent).toContain('将应用 1 / 1 块')

    const applyBtn = Array.from(host.querySelectorAll('button')).find((b) => b.textContent?.includes('应用 1 块'))
    expect(applyBtn).toBeTruthy()
    await act(async () => {
      ;(applyBtn as HTMLButtonElement).click()
      await Promise.resolve()
    })

    expect(apply).toHaveBeenCalledTimes(1)
    const body = apply.mock.calls[0][1]
    expect(body.base_hash).toBe('hash-1')
    expect(body.start_line).toBe(2)
    expect(body.old_text).toBe(RESULT.old_text)
    expect(body.decisions).toEqual([true])

    // The write stays inside the dialog until the user is done, because undo
    // lives here; the open file reloads from disk right away.
    expect(useStore.getState().aiEditOpen).toBe(true)
    expect(useStore.getState().aiEditApplied).toEqual({ applied: 1, rejected: 0 })
    expect(useStore.getState().fileReloadNonce).toBe(1)
    expect(host.textContent).toContain('已写回 1 块')

    const undo = vi.spyOn(api, 'aiEditUndo').mockResolvedValue({
      path: 'pkg/a.go',
      restored: true,
      new_content: 'package p\n\nfunc A() int {\n\treturn 1\n}\n',
    })
    const undoBtn = Array.from(host.querySelectorAll('button')).find((b) => b.textContent?.includes('撤销这次改写'))
    expect(undoBtn).toBeTruthy()
    await act(async () => {
      ;(undoBtn as HTMLButtonElement).click()
      await Promise.resolve()
    })
    expect(undo).toHaveBeenCalledTimes(1)
    expect(useStore.getState().aiEditOpen).toBe(false)
    expect(useStore.getState().fileReloadNonce).toBe(2)
  })

  it('rejects a hunk by clicking its header and sends decisions accordingly', async () => {
    vi.spyOn(api, 'aiEditStream').mockResolvedValue(RESULT)
    const apply = vi.spyOn(api, 'aiEditApply').mockResolvedValue({
      path: 'pkg/a.go',
      applied: 0,
      rejected: 1,
      new_content: '',
      conflict: false,
    })

    await render()
    await act(async () => {
      typeInto(host.querySelector('textarea') as HTMLTextAreaElement, '改一下')
      await Promise.resolve()
    })
    await act(async () => {
      ;(Array.from(host.querySelectorAll('button')).find((b) => b.textContent?.includes('生成候选')) as HTMLButtonElement).click()
      await Promise.resolve()
    })

    const header = Array.from(host.querySelectorAll('button')).find((b) => b.textContent?.includes('@@ 块 1 @@')) as HTMLButtonElement
    expect(header).toBeTruthy()
    await act(async () => {
      header.click()
      await Promise.resolve()
    })
    expect(host.textContent).toContain('已拒绝（点击恢复）')

    const applyBtn = Array.from(host.querySelectorAll('button')).find((b) => b.textContent?.includes('应用 0 块')) as HTMLButtonElement
    expect(applyBtn.disabled).toBe(true)
    // Applying is blocked when every hunk is rejected, so nothing can be
    // written by a stray Cmd-S.
    await act(async () => {
      header.click()
      await Promise.resolve()
    })
    const enabled = Array.from(host.querySelectorAll('button')).find((b) => b.textContent?.includes('应用 1 块')) as HTMLButtonElement
    await act(async () => {
      enabled.click()
      await Promise.resolve()
    })
    expect(apply.mock.calls[0][1].decisions).toEqual([true])
  })

  it('surfaces a generation error without losing the dialog', async () => {
    vi.spyOn(api, 'aiEditStream').mockRejectedValue(new Error('boom'))
    await render()
    await act(async () => {
      typeInto(host.querySelector('textarea') as HTMLTextAreaElement, '改一下')
      await Promise.resolve()
    })
    await act(async () => {
      ;(Array.from(host.querySelectorAll('button')).find((b) => b.textContent?.includes('生成候选')) as HTMLButtonElement).click()
      await Promise.resolve()
    })
    expect(host.textContent).toContain('boom')
    expect(useStore.getState().aiEditOpen).toBe(true)
  })

  // Cursor's inline chat: a follow-up must carry the earlier turn, otherwise
  // the model answers "再补一个零值短路" from the original code and throws
  // away the work the first turn did.
  it('carries the earlier turn into a follow-up', async () => {
    const bodies: AiEditRequest[] = []
    vi.spyOn(api, 'aiEditStream').mockImplementation(async (_id, body) => {
      bodies.push(body)
      return bodies.length === 1 ? RESULT : RESULT
    })

    await render()
    const ta = host.querySelector('textarea') as HTMLTextAreaElement
    await act(async () => {
      typeInto(ta, '把返回值改成 2')
      await Promise.resolve()
    })
    await act(async () => {
      ;(Array.from(host.querySelectorAll('button')).find((b) => b.textContent?.includes('生成候选')) as HTMLButtonElement).click()
      await Promise.resolve()
    })

    expect(bodies).toHaveLength(1)
    expect(bodies[0].history ?? []).toHaveLength(0)

    // The box is cleared after a turn, so the next instruction is clearly new.
    await act(async () => {
      typeInto(ta, '再补一个零值短路')
      await Promise.resolve()
    })
    const followUp = Array.from(host.querySelectorAll('button')).find((b) => b.textContent?.includes('继续改'))
    expect(followUp).toBeTruthy()
    await act(async () => {
      ;(followUp as HTMLButtonElement).click()
      await Promise.resolve()
    })

    expect(bodies).toHaveLength(2)
    const history = bodies[1].history ?? []
    expect(history).toHaveLength(1)
    expect(history[0].instruction).toBe('把返回值改成 2')
    expect(history[0].code).toBe(RESULT.new_text)
    // The dialog says which round this is, and offers a way back to the start.
    expect(host.textContent).toContain('第 2 轮')
    expect(Array.from(host.querySelectorAll('button')).some((b) => b.textContent?.includes('从头再来'))).toBe(true)
  })

  it('undoes repeatedly, and Cmd-Z reaches the stack', async () => {
    vi.spyOn(api, 'aiEditStream').mockResolvedValue(RESULT)
    vi.spyOn(api, 'aiEditApply').mockResolvedValue({
      path: 'pkg/a.go',
      applied: 1,
      rejected: 0,
      new_content: 'package p\n\nfunc A() int {\n\treturn 2\n}\n',
      conflict: false,
    })
    // The server says one more write can still be undone.
    const undo = vi.spyOn(api, 'aiEditUndo').mockResolvedValue({
      path: 'pkg/a.go',
      restored: true,
      new_content: 'package p\n\nfunc A() int {\n\treturn 1\n}\n',
      remaining: 2,
    })

    await render()
    await act(async () => {
      typeInto(host.querySelector('textarea') as HTMLTextAreaElement, '改一下')
      await Promise.resolve()
    })
    await act(async () => {
      ;(Array.from(host.querySelectorAll('button')).find((b) => b.textContent?.includes('生成候选')) as HTMLButtonElement).click()
      await Promise.resolve()
    })
    await act(async () => {
      ;(Array.from(host.querySelectorAll('button')).find((b) => b.textContent?.includes('应用 1 块')) as HTMLButtonElement).click()
      await Promise.resolve()
    })

    // The dialog offers the undo.
    const undoBtn = Array.from(host.querySelectorAll('button')).find((b) => b.textContent?.includes('撤销'))
    expect(undoBtn).toBeTruthy()

    await act(async () => {
      ;(undoBtn as HTMLButtonElement).click()
      await Promise.resolve()
    })
    expect(undo).toHaveBeenCalledTimes(1)
    // Writes are still undoable, so the dialog stays open and says how many.
    expect(useStore.getState().aiEditOpen).toBe(true)
    expect(useStore.getState().aiEditUndoDepth).toBe(2)
    expect(host.textContent).toContain('还有')

    // Cmd-Z is the editor's shortcut for the same thing.
    await act(async () => {
      window.dispatchEvent(new KeyboardEvent('keydown', { key: 'z', metaKey: true, bubbles: true }))
      await Promise.resolve()
    })
    expect(undo).toHaveBeenCalledTimes(2)
  })

  it('shows the change where it happens, with the surrounding lines', async () => {
    vi.spyOn(api, 'aiEditStream').mockResolvedValue({
      ...RESULT,
      context_before: 'package p\n\nfunc A() int {\n',
      context_after: '}\n',
    })
    await render()
    await act(async () => {
      typeInto(host.querySelector('textarea') as HTMLTextAreaElement, '改一下')
      await Promise.resolve()
    })
    await act(async () => {
      ;(Array.from(host.querySelectorAll('button')).find((b) => b.textContent?.includes('生成候选')) as HTMLButtonElement).click()
      await Promise.resolve()
    })

    expect(host.textContent).toContain('改动所在的位置')
    expect(host.textContent).toContain('func A() int {')
    // The proposal is marked, the context is not.
    const marked = Array.from(host.querySelectorAll('span')).find((sp) =>
      sp.className.includes('bg-emerald-950'),
    )
    expect(marked?.textContent).toContain('return 2')
  })

  it('keeps the earlier turns across dialog invocations, for a follow-up on another file', async () => {
    const bodies: AiEditRequest[] = []
    vi.spyOn(api, 'aiEditStream').mockImplementation(async (_id, body) => {
      bodies.push(body)
      return RESULT
    })

    await render()
    const ta = host.querySelector('textarea') as HTMLTextAreaElement
    await act(async () => {
      typeInto(ta, '改一下签名')
      await Promise.resolve()
    })
    await act(async () => {
      ;(Array.from(host.querySelectorAll('button')).find((b) => b.textContent?.includes('生成候选')) as HTMLButtonElement).click()
      await Promise.resolve()
    })

    // The user closes the dialog and opens it again on ANOTHER file.
    act(() => useStore.getState().closeAiEditDialog())
    await act(async () => {
      useStore.getState().openAiEditDialog('pkg/b.go', { start_line: 9, end_line: 11 })
      await Promise.resolve()
    })
    await render()
    const ta2 = host.querySelector('textarea') as HTMLTextAreaElement
    await act(async () => {
      typeInto(ta2, '调用方也要跟着改')
      await Promise.resolve()
    })
    await act(async () => {
      ;(Array.from(host.querySelectorAll('button')).find((b) => b.textContent?.includes('生成候选')) as HTMLButtonElement).click()
      await Promise.resolve()
    })

    expect(bodies).toHaveLength(2)
    const history = bodies[1].history ?? []
    expect(history).toHaveLength(1)
    expect(history[0].instruction).toBe('改一下签名')
    // The turn knows which file it was about, so the server does not paste it
    // into the new file.
    expect(history[0].path).toBe('pkg/a.go')
  })

  it('drops the earlier turns when the user starts over', async () => {
    const bodies: AiEditRequest[] = []
    vi.spyOn(api, 'aiEditStream').mockImplementation(async (_id, body) => {
      bodies.push(body)
      return RESULT
    })

    await render()
    const ta = host.querySelector('textarea') as HTMLTextAreaElement
    await act(async () => {
      typeInto(ta, '第一轮')
      await Promise.resolve()
    })
    await act(async () => {
      ;(Array.from(host.querySelectorAll('button')).find((b) => b.textContent?.includes('生成候选')) as HTMLButtonElement).click()
      await Promise.resolve()
    })

    const reset = Array.from(host.querySelectorAll('button')).find((b) => b.textContent?.includes('从头再来'))
    expect(reset).toBeTruthy()
    await act(async () => {
      ;(reset as HTMLButtonElement).click()
      await Promise.resolve()
    })
    expect(useStore.getState().aiEditHistory).toHaveLength(0)

    await act(async () => {
      typeInto(ta, '重新来一遍')
      await Promise.resolve()
    })
    await act(async () => {
      ;(Array.from(host.querySelectorAll('button')).find((b) => b.textContent?.includes('生成候选')) as HTMLButtonElement).click()
      await Promise.resolve()
    })
    expect(bodies).toHaveLength(2)
    expect(bodies[1].history ?? []).toHaveLength(0)
  })
})

describe('AiEditDialog docking', () => {
  let root: Root
  let host: HTMLElement
  // This file's jsdom environment ships a localStorage object without working
  // methods, so the docking tests run against a Map-backed stub.
  const store: Record<string, string> = {}

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
      aiEditSelection: { start_line: 2, end_line: 3 },
      aiEditResult: null,
      aiEditStreaming: '',
      aiEditBusy: false,
      aiEditApplied: null,
      aiEditHistory: [],
      aiEditUndoDepth: 0,
    })
  })

  afterEach(() => {
    act(() => root.unmount())
    host.remove()
    vi.unstubAllGlobals()
    vi.restoreAllMocks()
  })

  async function render() {
    await act(async () => {
      root.render(<AiEditDialog />)
      await Promise.resolve()
    })
  }

  function header(): HTMLElement {
    const h = [...host.querySelectorAll('div')].find((d) => (d.textContent ?? '').includes('AI 改写') && (d.className ?? '').includes('cursor-move'))
    if (!h) throw new Error('drag header missing')
    return h
  }

  function panel(): HTMLElement {
    const p = host.querySelector('.pointer-events-auto') as HTMLElement
    if (!p) throw new Error('panel missing')
    return p
  }

  it('stays docked until dragged, then follows the pointer and persists', async () => {
    await render()
    expect(panel().style.position).toBe('')

    // Drag the header: mousedown, move, up.
    const h = header()
    await act(async () => {
      h.dispatchEvent(new MouseEvent('mousedown', { bubbles: true, button: 0, clientX: 1000, clientY: 300 }))
      window.dispatchEvent(new MouseEvent('mousemove', { clientX: 800, clientY: 200 }))
      window.dispatchEvent(new MouseEvent('mouseup', { clientX: 800, clientY: 200 }))
    })
    expect(panel().style.position).toBe('absolute')
    expect(panel().style.right).toBeTruthy()
    expect(localStorage.getItem('divvy.aiEditPos')).toBeTruthy()

    // A remount restores the persisted position.
    await act(async () => {
      root.unmount()
    })
    root = createRoot(host)
    await act(async () => {
      root.render(<AiEditDialog />)
      await Promise.resolve()
    })
    expect(panel().style.position).toBe('absolute')
    expect(panel().style.right).toBeTruthy()
  })

  it('double-click on the header returns the dialog to the docked spot', async () => {
    await render()
    const h = header()
    await act(async () => {
      h.dispatchEvent(new MouseEvent('mousedown', { bubbles: true, button: 0, clientX: 1000, clientY: 300 }))
      window.dispatchEvent(new MouseEvent('mousemove', { clientX: 700, clientY: 150 }))
      window.dispatchEvent(new MouseEvent('mouseup', { clientX: 700, clientY: 150 }))
    })
    expect(panel().style.position).toBe('absolute')

    await act(async () => {
      h.dispatchEvent(new MouseEvent('dblclick', { bubbles: true }))
    })
    expect(panel().style.position).toBe('')
    expect(localStorage.getItem('divvy.aiEditPos')).toBeFalsy()
  })

  it('多轮上下文 transcript renders prior turns with expandable code', async () => {
    await render()
    // Seeded inside this test: two prior rounds for the open dialog.
    await act(async () => {
      useStore.setState({
        aiEditHistory: [
          {
            instruction: '加文档注释',
            code: 'func A() int { // doc }',
            path: 'pkg/a.go',
            applied: true,
            files: [{ path: 'pkg/a.go', code: 'func A() int { // doc }', applied: true }],
          },
          {
            instruction: '再补一个超时分支',
            code: 'func A() int { ... timeout ... }',
            path: 'pkg/a.go',
            files: [{ path: 'pkg/a.go', code: 'func A() int { ... timeout ... }', applied: false }],
          },
        ],
      })
      await Promise.resolve()
    })
    expect(host.textContent).toContain('多轮上下文')
    expect(host.textContent).toContain('第 1 轮')
    expect(host.textContent).toContain('加文档注释')
    expect(host.textContent).toContain('再补一个超时分支')
    // Collapsed: the produced code is hidden until a turn is expanded.
    expect(host.textContent).not.toContain('// doc }')

    const rows = [...host.querySelectorAll('button')].filter((b) => (b.textContent || '').includes('第 2 轮'))
    await act(async () => {
      rows[0].dispatchEvent(new MouseEvent('click', { bubbles: true }))
    })
    expect(host.textContent).toContain('... timeout ...')
    expect(host.textContent).toContain('待应用')

    await act(async () => {
      rows[0].dispatchEvent(new MouseEvent('click', { bubbles: true }))
    })
    expect(host.textContent).not.toContain('... timeout ...')
  })
})
