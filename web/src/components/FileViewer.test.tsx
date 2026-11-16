/** @vitest-environment jsdom */

import { act } from 'react'
import { createRoot, type Root } from 'react-dom/client'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { api } from '../api/client'
import { useStore } from '../store'
import { FileViewer } from './FileViewer'

const FILE = 'pkg/add.go'
const CONTENT = 'line one\nline two\nline three\nline four\nline five\n'

describe('FileViewer search reveal and draft protection', () => {
  let root: Root
  let host: HTMLElement

  beforeEach(() => {
    host = document.createElement('div')
    document.body.appendChild(host)
    root = createRoot(host)
    ;(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true
    vi.spyOn(api, 'fsFile').mockResolvedValue({
      path: FILE,
      content: CONTENT,
      size: CONTENT.length,
      hash: 'hash-1',
      editable: true,
      mode: 0o644,
    })
    useStore.setState({
      currentConvId: 'conv-a',
      currentId: 'sess-a',
      conversation: null,
      desiredConvId: undefined,
      convNavGen: 0,
      sessionNavGen: 0,
      pendingSelections: [],
      pendingSelectionsByConv: {},
      fileDirty: false,
      pendingFileNav: null,
      loadTurnChanges: async () => {},
      fileView: {
        path: FILE,
        sessionId: 'sess-a',
        preferDiff: true,
        line: 1,
        revealNonce: 1,
      },
      turnChanges: {
        sessionId: 'sess-a',
        changes: [
          {
            path: FILE,
            status: 'modified',
            old_content: 'old body',
            new_content: 'UNIQUE_DIFF_BODY',
          },
        ],
      },
    })
  })

  afterEach(() => {
    act(() => {
      root.unmount()
    })
    host.remove()
    vi.restoreAllMocks()
  })

  async function renderViewer() {
    await act(async () => {
      root.render(<FileViewer sessionId="sess-a" path={FILE} />)
      await Promise.resolve()
    })
  }

  it('leaves the turn-diff view and reveals the current file when search requests preferDiff false', async () => {
    await renderViewer()
    expect(host.textContent).toMatch(/下面是这一轮对该文件的改动/)
    expect(host.querySelector('textarea')).toBeNull()

    await act(async () => {
      useStore.getState().openFile(FILE, 'sess-a', { line: 4, preferDiff: false })
      await Promise.resolve()
    })

    expect(host.textContent).not.toMatch(/下面是这一轮对该文件的改动/)
    const ta = host.querySelector('textarea') as HTMLTextAreaElement
    expect(ta).toBeTruthy()
    expect(ta.getAttribute('wrap')).toBe('off')
    expect(ta.className).toMatch(/text-\[12px\]/)
    expect(ta.className).toMatch(/leading-\[1\.6\]/)
    const gutter = host.querySelector('[aria-hidden]') as HTMLElement
    expect(gutter.className).toMatch(/text-\[12px\]/)
    expect(gutter.className).toMatch(/leading-\[1\.6\]/)
    expect(useStore.getState().fileView?.preferDiff).toBe(false)
    expect(useStore.getState().fileView?.line).toBe(4)
  })

  it('keeps an unsaved draft on same-file reveal and asks before leaving for another file', async () => {
    useStore.setState({
      fileView: { path: FILE, sessionId: 'sess-a', preferDiff: false, line: 1, revealNonce: 1 },
    })
    await renderViewer()
    const ta = host.querySelector('textarea') as HTMLTextAreaElement
    expect(ta).toBeTruthy()

    await act(async () => {
      const native = Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, 'value')
      native?.set?.call(ta, 'draft change\n' + CONTENT)
      ta.dispatchEvent(new Event('input', { bubbles: true }))
      await Promise.resolve()
    })
    expect(useStore.getState().fileDirty).toBe(true)

    await act(async () => {
      useStore.getState().openFile(FILE, 'sess-a', { line: 5, preferDiff: false })
      await Promise.resolve()
    })
    const still = host.querySelector('textarea') as HTMLTextAreaElement
    expect(still.value).toMatch(/draft change/)
    expect(host.textContent).toMatch(/未保存草稿/)
    expect(useStore.getState().pendingFileNav).toBeNull()

    await act(async () => {
      useStore.getState().openFile('pkg/other.go', 'sess-a', { line: 1, preferDiff: false })
    })
    expect(useStore.getState().pendingFileNav).toMatchObject({ path: 'pkg/other.go' })
    expect(host.textContent).toMatch(/未保存的修改/)

    await act(async () => {
      const keep = Array.from(host.querySelectorAll('button')).find((b) => b.textContent === '继续编辑') as HTMLButtonElement
      keep.click()
    })
    expect(useStore.getState().pendingFileNav).toBeNull()
    expect(useStore.getState().fileView?.path).toBe(FILE)
    expect((host.querySelector('textarea') as HTMLTextAreaElement).value).toMatch(/draft change/)
  })
})

// A pending AI rewrite is reviewed IN PLACE: the panel shows the file with the
// change applied, so the decision is made where the code is.
describe('FileViewer AI rewrite preview', () => {
  let root: Root
  let host: HTMLElement

  beforeEach(() => {
    host = document.createElement('div')
    document.body.appendChild(host)
    root = createRoot(host)
    ;(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true
    vi.spyOn(api, 'fsFile').mockResolvedValue({
      path: FILE,
      content: CONTENT,
      size: CONTENT.length,
      hash: 'hash-1',
      editable: true,
      mode: 0o644,
    })
    useStore.setState({
      currentConvId: 'conv-a',
      fileView: { path: FILE, sessionId: 'sess-a', line: 1, revealNonce: 1 },
      aiEditOpen: true,
      aiEditPath: FILE,
      aiEditResult: {
        path: FILE,
        base_hash: 'hash-1',
        start_line: 2,
        end_line: 2,
        old_text: 'line two\n',
        new_text: 'line two rewritten\n',
        hunks: [
          {
            index: 0,
            lines: [
              { type: 'remove', old_no: 2, text: 'line two' },
              { type: 'add', new_no: 2, text: 'line two rewritten' },
            ],
          },
        ],
        preview: '@@ 块 1 @@',
      },
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

  it('shows the file with the rewrite applied, and the changed line marked', async () => {
    await act(async () => {
      root.render(<FileViewer sessionId="sess-a" path={FILE} />)
      await Promise.resolve()
    })
    await act(async () => {
      await Promise.resolve()
    })

    expect(host.textContent).toContain('AI 改写预览')
    // The proposed text is what is shown.
    expect(host.textContent).toContain('line two rewritten')
    // And the changed line is marked.
    const marked = Array.from(host.querySelectorAll('span')).find((sp) =>
      sp.className.includes('bg-emerald-950'),
    )
    expect(marked?.textContent).toContain('line two rewritten')
  })

  it('in a multi-file round, previews the proposal for the file actually open', async () => {
    const other = {
      path: 'pkg/other.go',
      base_hash: 'hash-other',
      start_line: 2,
      end_line: 2,
      old_text: 'line two\n',
      new_text: 'OTHER_FILE_REWRITE\n',
      hunks: [
        {
          index: 0,
          lines: [
            { type: 'remove', old_no: 2, text: 'line two' },
            { type: 'add', new_no: 2, text: 'OTHER_FILE_REWRITE' },
          ],
        },
      ],
      preview: '@@ 块 1 @@',
    }
    useStore.setState({
      aiEditResult: {
        ...other,
        proposals: [
          other,
          {
            path: FILE,
            base_hash: 'hash-1',
            start_line: 2,
            end_line: 2,
            old_text: 'line two\n',
            new_text: 'THIS_FILE_REWRITE\n',
            hunks: [
              {
                index: 0,
                lines: [
                  { type: 'remove', old_no: 2, text: 'line two' },
                  { type: 'add', new_no: 2, text: 'THIS_FILE_REWRITE' },
                ],
              },
            ],
            preview: '@@ 块 1 @@',
          },
        ],
      },
    })
    await act(async () => {
      root.render(<FileViewer sessionId="sess-a" path={FILE} />)
      await Promise.resolve()
    })
    expect(host.textContent).toContain('THIS_FILE_REWRITE')
    expect(host.textContent).not.toContain('OTHER_FILE_REWRITE')
  })

  it('the toolbar button joins the open dialog round instead of starting a new one', async () => {
    useStore.setState({
      aiEditResult: null,
      aiEditTargets: [{ path: 'pkg/a.go', selection: { start_line: 1, end_line: 1 } }],
      aiEditPath: 'pkg/a.go',
    })
    await act(async () => {
      root.render(<FileViewer sessionId="sess-a" path={FILE} />)
      await Promise.resolve()
    })
    const join = Array.from(host.querySelectorAll('button')).find((b) => b.textContent?.includes('加入改写'))
    expect(join).toBeTruthy()
    await act(async () => {
      ;(join as HTMLButtonElement).click()
      await Promise.resolve()
    })
    const targets = useStore.getState().aiEditTargets
    expect(targets).toHaveLength(2)
    expect(targets[1].path).toBe(FILE)
  })
})

describe('FileViewer ghost diff during a live run', () => {
  let root: Root
  let host: HTMLElement

  beforeEach(() => {
    host = document.createElement('div')
    document.body.appendChild(host)
    root = createRoot(host)
    ;(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true
    vi.spyOn(api, 'fsFile').mockResolvedValue({
      path: FILE,
      content: CONTENT,
      size: CONTENT.length,
      hash: 'hash-1',
      editable: true,
      mode: 0o644,
    })
    useStore.setState({
      currentConvId: 'conv-a',
      currentId: 'sess-a',
      conversation: null,
      desiredConvId: undefined,
      convNavGen: 0,
      sessionNavGen: 0,
      pendingSelections: [],
      pendingSelectionsByConv: {},
      fileDirty: false,
      pendingFileNav: null,
      loadTurnChanges: async () => {},
      fileView: { path: FILE, sessionId: 'sess-a', preferDiff: false },
      turnChanges: null,
      live: { session_id: 'sess-a', phase: 'running', approval_mode: 'auto', workdir: '/tmp' },
    })
  })

  afterEach(() => {
    act(() => {
      root.unmount()
    })
    host.remove()
    vi.restoreAllMocks()
  })

  async function renderViewer() {
    await act(async () => {
      root.render(<FileViewer sessionId="sess-a" path={FILE} />)
      await Promise.resolve()
    })
  }

  function diffVisible(): boolean {
    return (host.textContent ?? '').includes('下面是这一轮对该文件的改动')
  }

  async function setChanges(changes: Record<string, unknown>[]) {
    await act(async () => {
      useStore.setState({
        turnChanges: { sessionId: 'sess-a', changes: changes as never, editable: false },
      })
      await Promise.resolve()
    })
  }

  it('flips to the diff when the turn edits the open file, honors the manual switch, and flips again on the next edit', async () => {
    await renderViewer()
    // No change yet: editor mode, and the ghost did not fire.
    expect(diffVisible()).toBe(false)

    // First sight of a change: remember, but do not yank.
    await setChanges([{ path: FILE, status: 'modified', old_content: 'a', new_content: 'UNIQUE_V1' }])
    await act(async () => {
      await Promise.resolve()
    })
    expect(diffVisible()).toBe(false)

    // The edit grows (second version): ghost diff surfaces, live run.
    await setChanges([{ path: FILE, status: 'modified', old_content: 'a', new_content: 'UNIQUE_V2' }])
    await act(async () => {
      await Promise.resolve()
    })
    expect(diffVisible()).toBe(true)

    // Manual switch back to the editor is honored.
    await act(async () => {
      const btn = [...host.querySelectorAll('button')].find((b) => b.textContent?.trim() === '当前文件')
      btn!.click()
    })
    expect(diffVisible()).toBe(false)

    // The next edit wins again: the diff is what the user is here for.
    await setChanges([{ path: FILE, status: 'modified', old_content: 'a', new_content: 'UNIQUE_V3' }])
    await act(async () => {
      await Promise.resolve()
    })
    expect(diffVisible()).toBe(true)
  })

  it('does not ghost when the turn has ended', async () => {
    useStore.setState({
      live: { session_id: 'sess-a', phase: 'done', approval_mode: 'auto', workdir: '/tmp' },
    })
    await renderViewer()
    await setChanges([{ path: FILE, status: 'modified', old_content: 'a', new_content: 'UNIQUE_V1' }])
    await act(async () => {
      await Promise.resolve()
    })
    expect(diffVisible()).toBe(false)
    await setChanges([{ path: FILE, status: 'modified', old_content: 'a', new_content: 'UNIQUE_V2' }])
    await act(async () => {
      await Promise.resolve()
    })
    expect(diffVisible()).toBe(false)
  })

  it('lists the turn\u2019s other touched files with a one-click hop', async () => {
    await renderViewer()
    await setChanges([
      { path: FILE, status: 'modified', old_content: 'a', new_content: 'UNIQUE_V1' },
      { path: 'pkg/other.go', status: 'added', old_content: '', new_content: 'other body' },
    ])
    await act(async () => {
      await Promise.resolve()
    })
    const hop = [...host.querySelectorAll('button')].find((b) => b.textContent?.includes('pkg/other.go'))
    expect(hop).toBeTruthy()
    expect(hop!.textContent).toContain('新增')

    await act(async () => {
      hop!.click()
    })
    expect(useStore.getState().fileView?.path).toBe('pkg/other.go')
    expect(useStore.getState().fileView?.preferDiff).toBe(true)
  })
})
