import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { api } from './api/client'
import { useStore } from './store'
import type { AiEditApplyRequest, AiEditProposal, AiEditRequest, AiEditResponse, ReviewHunk } from './types'

const hunk = (text: string): ReviewHunk => ({
  index: 0,
  lines: [
    { type: 'remove', old_no: 2, text: 'old' },
    { type: 'add', new_no: 2, text: text },
  ],
})

const proposal = (path: string, text: string): AiEditProposal => ({
  path,
  base_hash: `hash-${path}`,
  start_line: 2,
  end_line: 2,
  old_text: 'old\n',
  new_text: text,
  hunks: [hunk(text)],
  preview: '@@ 块 1 @@',
})

const MULTI: AiEditResponse = {
  path: 'a.go',
  base_hash: 'hash-a.go',
  start_line: 2,
  end_line: 2,
  old_text: 'old\n',
  new_text: 'A2\n',
  hunks: [hunk('A2')],
  preview: '@@ 块 1 @@',
  proposals: [proposal('a.go', 'A2\n'), proposal('b.go', 'B2\n')],
}

const targets = [
  { path: 'a.go', selection: { start_line: 2, end_line: 2 } },
  { path: 'b.go', selection: { start_line: 3, end_line: 4 } },
]

describe('multi-file ⌘K store', () => {
  beforeEach(() => {
    useStore.setState({
      currentConvId: 'conv-a',
      actionError: null,
      aiEditOpen: true,
      aiEditPath: 'a.go',
      aiEditSelection: { start_line: 2, end_line: 2 },
      aiEditTargets: targets,
      aiEditResult: null,
      aiEditStreaming: '',
      aiEditProgress: '',
      aiEditBusy: false,
      aiEditApplied: null,
      aiEditHistory: [],
      aiEditUndoDepth: 0,
      fileReloadNonce: 0,
    })
  })

  afterEach(() => vi.restoreAllMocks())

  it('sends the explicit target list and records one turn with one entry per file', async () => {
    const bodies: AiEditRequest[] = []
    vi.spyOn(api, 'aiEditStream').mockImplementation(async (_id, body, _onDelta, _signal, onProgress) => {
      bodies.push(body)
      onProgress?.(1, 2, 'a.go')
      onProgress?.(2, 2, 'b.go')
      return MULTI
    })

    const ok = await useStore.getState().runAiEdit('一起改')
    expect(ok).toBe(true)
    expect(bodies).toHaveLength(1)
    expect(bodies[0].targets).toEqual(targets)
    expect(bodies[0].path).toBeUndefined()
    expect(useStore.getState().aiEditHistory).toHaveLength(1)
    expect(useStore.getState().aiEditHistory[0].files).toEqual([
      { path: 'a.go', code: 'A2\n' },
      { path: 'b.go', code: 'B2\n' },
    ])
    expect(useStore.getState().aiEditProgress).toBe('')
  })

  it('keeps the legacy single-file request shape when there is one target', async () => {
    useStore.setState({
      aiEditTargets: [targets[0]],
      aiEditPath: 'a.go',
      aiEditSelection: { start_line: 2, end_line: 2 },
    })
    const bodies: AiEditRequest[] = []
    vi.spyOn(api, 'aiEditStream').mockImplementation(async (_id, body) => {
      bodies.push(body)
      return { ...MULTI, proposals: undefined }
    })
    await useStore.getState().runAiEdit('只改一个')
    expect(bodies[0].path).toBe('a.go')
    expect(bodies[0].selection).toEqual({ start_line: 2, end_line: 2 })
    expect(bodies[0].targets).toBeUndefined()
    expect(useStore.getState().aiEditHistory[0].files).toBeUndefined()
  })

  it('adds/updates/removes targets and drops stale proposals', async () => {
    useStore.setState({ aiEditResult: MULTI })
    useStore.getState().addAiEditTarget('b.go', { start_line: 9, end_line: 10 })
    expect(useStore.getState().aiEditTargets).toHaveLength(2)
    expect(useStore.getState().aiEditTargets[1].selection).toEqual({ start_line: 9, end_line: 10 })
    expect(useStore.getState().aiEditResult).toBeNull()

    useStore.getState().addAiEditTarget('c.go', { start_line: 1, end_line: 1 })
    expect(useStore.getState().aiEditTargets.map((t) => t.path)).toEqual(['a.go', 'b.go', 'c.go'])
    useStore.getState().removeAiEditTarget('c.go')
    expect(useStore.getState().aiEditTargets.map((t) => t.path)).toEqual(['a.go', 'b.go'])
  })

  it('sends per-file decisions and marks only the files actually written', async () => {
    useStore.setState({
      aiEditResult: MULTI,
      aiEditHistory: [
        {
          instruction: '一起改',
          code: 'A2\n',
          path: 'a.go',
          files: [
            { path: 'a.go', code: 'A2\n' },
            { path: 'b.go', code: 'B2\n' },
          ],
        },
      ],
    })
    const bodies: AiEditApplyRequest[] = []
    vi.spyOn(api, 'aiEditApply').mockImplementation(async (_id, body) => {
      bodies.push(body)
      return {
        path: 'a.go',
        applied: 1,
        rejected: 1,
        new_content: 'A2\n',
        conflict: false,
        paths: ['a.go'],
      }
    })

    const ok = await useStore.getState().applyAiEdit({ 'a.go': [true], 'b.go': [false] })
    expect(ok).toBe(true)
    expect(bodies).toHaveLength(1)
    expect(bodies[0].proposals?.map((p) => p.path)).toEqual(['a.go', 'b.go'])
    expect(bodies[0].proposals?.[0].decisions).toEqual([true])
    expect(bodies[0].proposals?.[1].decisions).toEqual([false])
    // b.go was rejected: its proposal stays pending, so a follow-up can
    // still revise it instead of starting from the disk content.
    const files = useStore.getState().aiEditHistory[0].files ?? []
    expect(files[0].applied).toBe(true)
    expect(files[1].applied).toBeFalsy()
    expect(useStore.getState().aiEditUndoDepth).toBe(1)
  })

  it('refuses to apply a round where every file failed to generate', async () => {
    useStore.setState({
      aiEditResult: {
        ...MULTI,
        proposals: [
          { ...proposal('a.go', 'old\n'), error: 'LLM 调用失败' },
          { ...proposal('b.go', 'old\n'), error: 'LLM 调用失败' },
        ],
      },
    })
    const apply = vi.spyOn(api, 'aiEditApply')
    expect(await useStore.getState().applyAiEdit()).toBe(false)
    expect(apply).not.toHaveBeenCalled()
    expect(useStore.getState().actionError).toContain('没有可写回')
  })
})
