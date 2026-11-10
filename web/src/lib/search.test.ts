import { describe, expect, it } from 'vitest'
import { citationFromSearchHit, draftLocateNotice, hitLabel, previewMatchesSourceLine, projectSearchCaption, sourceLineAt } from './search'
import { applySearchResult, beginSearch, canActOnSearchHits, emptySearchView, invalidateSearch, sameSearchScope } from './projectSearchSession'
import { planFileOpen, scrollTopForLine } from './fileNav'

describe('citationFromSearchHit', () => {
  it('reads the current source line instead of the truncated preview', async () => {
    const full = 'func Add(a, b int) int { return a + b } // this comment continues well past the search preview cut'
    const preview = full.slice(0, 40).trim() + '...'
    const got = await citationFromSearchHit(
      { path: 'pkg/add.go', line: 3, preview },
      async () => ({ content: `package pkg\n\n${full}\n` }),
    )
    expect(got.status).toBe('ok')
    expect(got.selection).toEqual({
      path: 'pkg/add.go',
      start_line: 3,
      end_line: 3,
      text: full,
      draft: false,
    })
    expect(got.selection?.text).not.toBe(preview)
    expect(got.notice).toMatch(/当前源行/)
    expect(hitLabel({ path: 'pkg/add.go', line: 3, preview })).toBe('pkg/add.go:L3')
  })

  it('detects a changed file and cites the current disk line, not the stale preview', async () => {
    const got = await citationFromSearchHit(
      { path: 'pkg/add.go', line: 1, preview: 'func Add(a, b int) int { return a - b }' },
      async () => ({ content: 'func Add(a, b int) int { return a + b }\n' }),
    )
    expect(got.status).toBe('changed')
    expect(got.selection?.text).toBe('func Add(a, b int) int { return a + b }')
    expect(got.notice).toMatch(/文件已变化/)
  })

  it('does not fall back to the preview when the source line is gone', async () => {
    const got = await citationFromSearchHit(
      { path: 'pkg/add.go', line: 9, preview: 'func Add() {}' },
      async () => ({ content: 'package pkg\n' }),
    )
    expect(got.status).toBe('missing')
    expect(got.selection).toBeNull()
    expect(got.notice).toMatch(/搜索预览不能代替源行/)
  })

  it('does not invent a citation when the file cannot be read', async () => {
    const got = await citationFromSearchHit(
      { path: 'pkg/add.go', line: 1, preview: 'secret-looking preview' },
      async () => {
        throw new Error('file missing')
      },
    )
    expect(got.status).toBe('error')
    expect(got.selection).toBeNull()
    expect(got.notice).toMatch(/未使用搜索预览代替/)
  })
})

describe('sourceLineAt and preview matching', () => {
  it('indexes 1-based lines and treats a clipped preview as a faithful prefix', () => {
    expect(sourceLineAt('a\nb\nc\n', 2)).toBe('b')
    expect(previewMatchesSourceLine('hello world', '  hello world  ')).toBe(true)
    const long = 'x'.repeat(250)
    expect(previewMatchesSourceLine(long.slice(0, 200).trim() + '...', long)).toBe(true)
    expect(previewMatchesSourceLine('old...', 'brand new line')).toBe(false)
  })
})

describe('projectSearchCaption', () => {
  it('states that search is literal and not a semantic index or LSP', () => {
    expect(projectSearchCaption).toMatch(/字面搜索/)
    expect(projectSearchCaption).toMatch(/不是语义索引/)
    expect(projectSearchCaption).toMatch(/语言服务/)
  })
})

describe('draftLocateNotice', () => {
  it('distinguishes a disk search hit from the unsaved draft buffer', () => {
    const text = draftLocateNotice('pkg/add.go', 4)
    expect(text).toMatch(/磁盘/)
    expect(text).toMatch(/pkg\/add\.go/)
    expect(text).toMatch(/未保存草稿/)
    expect(text).toMatch(/第 4 行/)
  })
})

describe('scrollTopForLine', () => {
  it('maps a 1-based line to a pixel offset in the editor', () => {
    expect(scrollTopForLine(1, 20)).toBe(0)
    expect(scrollTopForLine(3, 20)).toBe(40)
    expect(scrollTopForLine(0, 20)).toBe(0)
  })
})

describe('planFileOpen', () => {
  const cur = { path: 'a.ts', sessionId: 's1' as string | null }

  it('reveals a line in the current file without discarding a dirty draft', () => {
    const got = planFileOpen({ ...cur, revealNonce: 2 }, { path: 'a.ts', sessionId: 's1', line: 9 }, true)
    expect(got.kind).toBe('reveal')
    expect(got.view.line).toBe(9)
    expect(got.view.revealNonce).toBe(3)
  })

  it('leaves a same-file diff view when the opener requests preferDiff false', () => {
    const got = planFileOpen(
      { ...cur, preferDiff: true, revealNonce: 1 },
      { path: 'a.ts', sessionId: 's1', line: 4, preferDiff: false },
      true,
    )
    expect(got.kind).toBe('reveal')
    expect(got.view.preferDiff).toBe(false)
    expect(got.view.line).toBe(4)
  })

  it('asks for leave confirmation when opening a different file while dirty', () => {
    const got = planFileOpen(cur, { path: 'b.ts', sessionId: 's1', line: 1 }, true)
    expect(got.kind).toBe('confirm')
    expect(got.view.path).toBe('b.ts')
    expect(got.view.line).toBe(1)
  })

  it('opens a different file immediately when there is no draft', () => {
    const got = planFileOpen(cur, { path: 'b.ts', sessionId: 's1' }, false)
    expect(got.kind).toBe('open')
    expect(got.view.path).toBe('b.ts')
  })
})

describe('projectSearchSession isolation', () => {
  const a = { conversationId: 'conv-a', sessionId: 'sess-a' }
  const b = { conversationId: 'conv-b', sessionId: 'sess-b' }

  it('drops stale hits and ignores a late response after the scope changes', () => {
    expect(sameSearchScope(a, b)).toBe(false)
    let view = emptySearchView(a, 1)
    view = beginSearch(view, 1, a)
    view = applySearchResult(view, 1, a, {
      hits: [{ path: 'pkg/a.go', line: 1, preview: 'from A' }],
      truncated: false,
      scanned: 1,
    })
    expect(canActOnSearchHits(view, a)).toBe(true)

    view = invalidateSearch(view, b, 2)
    expect(view.hits).toBeNull()
    expect(canActOnSearchHits(view, b)).toBe(false)

    const ignored = applySearchResult(view, 1, a, {
      hits: [{ path: 'pkg/a.go', line: 1, preview: 'late A' }],
      truncated: false,
      scanned: 1,
    })
    expect(ignored).toBe(view)
    expect(ignored.hits).toBeNull()
  })
})
