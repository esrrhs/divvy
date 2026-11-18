import { describe, expect, it } from 'vitest'
import { langToFileExt, parseInline, parseMarkdown } from './markdown'

describe('parseMarkdown', () => {
  it('splits fenced code blocks with their language tag', () => {
    const blocks = parseMarkdown('before\n```go\nfunc Add() {}\n```\nafter')
    expect(blocks).toEqual([
      { kind: 'p', spans: [{ t: 'text', s: 'before' }] },
      { kind: 'code', lang: 'go', code: 'func Add() {}' },
      { kind: 'p', spans: [{ t: 'text', s: 'after' }] },
    ])
  })

  it('renders an unclosed fence as code instead of garbled paragraphs', () => {
    const blocks = parseMarkdown('```ts\nconst x = 1')
    expect(blocks).toEqual([{ kind: 'code', lang: 'ts', code: 'const x = 1' }])
  })

  it('keeps multi-line code blocks verbatim', () => {
    const blocks = parseMarkdown('```\na\nb\n```\n')
    expect(blocks).toEqual([{ kind: 'code', lang: '', code: 'a\nb' }])
  })

  it('parses headings, bullets, quotes and rules', () => {
    const blocks = parseMarkdown('## 计划\n- 第一步\n> 引用\n---\n正文')
    expect(blocks.map((b) => b.kind)).toEqual(['h', 'li', 'quote', 'hr', 'p'])
    const h = blocks[0] as Extract<typeof blocks[0], { kind: 'h' }>
    expect(h.level).toBe(2)
  })

  it('merges consecutive paragraph lines and splits on blank lines', () => {
    const blocks = parseMarkdown('第一行\n第二行\n\n第三行')
    expect(blocks).toHaveLength(2)
    const p = blocks[0] as Extract<typeof blocks[0], { kind: 'p' }>
    expect(p.spans).toEqual([{ t: 'text', s: '第一行\n第二行' }])
  })

  it('never throws on empty or undefined-ish input', () => {
    expect(parseMarkdown('')).toEqual([])
  })
})

describe('parseInline', () => {
  it('splits inline code and bold out of plain text', () => {
    expect(parseInline('改 `a.go` 并保持 **不变**')).toEqual([
      { t: 'text', s: '改 ' },
      { t: 'code', s: 'a.go' },
      { t: 'text', s: ' 并保持 ' },
      { t: 'bold', s: '不变' },
    ])
  })

  it('returns the whole text as one span when no markers exist', () => {
    expect(parseInline('plain')).toEqual([{ t: 'text', s: 'plain' }])
  })

  it('leaves unmatched markers as plain text', () => {
    expect(parseInline('a * b ** c')).toEqual([{ t: 'text', s: 'a * b ** c' }])
  })
})

describe('langToFileExt', () => {
  it('maps common language tags and falls back to .txt', () => {
    expect(langToFileExt('go')).toBe('.go')
    expect(langToFileExt('TypeScript')).toBe('.ts')
    expect(langToFileExt('bash')).toBe('.sh')
    expect(langToFileExt('')).toBe('.txt')
    expect(langToFileExt('brainfuck')).toBe('.txt')
  })
})
import { clampDialogPos } from './conversation'

describe('clampDialogPos', () => {
  it('keeps the panel reachable at the viewport edges', () => {
    expect(clampDialogPos(100, 100, 540, 400, 1280, 720)).toEqual({ top: 100, right: 100 })
    expect(clampDialogPos(-50, -50, 540, 400, 1280, 720).top).toBe(8)
    expect(clampDialogPos(5000, 5000, 540, 400, 1280, 720).right).toBe(1280 - 540 - 8)
    expect(clampDialogPos(Number.NaN, Number.NaN, 540, 400, 1280, 720)).toEqual({ top: 8, right: 8 })
  })
})
