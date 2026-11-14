import { describe, expect, it } from 'vitest'
import { LangFromPath, tokenizeLines } from './highlight'

function kinds(line: string, lang: 'go' | 'ts' | 'python' | 'json' | 'shell'): string[] {
  return tokenizeLines(line, lang)
    .flat()
    .map((t) => t.kind)
}

describe('tokenizeLines', () => {
  it('recognises Go keywords, strings, comments and numbers', () => {
    const line = 'func Add(a int) int { return a + 1 } // sums'
    expect(kinds(line, 'go')).toContain('keyword')
    expect(kinds(line, 'go')).toContain('number')
    expect(kinds(line, 'go')).toContain('comment')
  })

  it('never changes the text: the tokens concatenate back to the source', () => {
    const src = [
      'package main',
      '',
      'import "fmt"',
      '',
      '// Add returns the sum.',
      'func Add(a, b int) int {',
      '\treturn a + b // inline',
      '}',
      '',
      'func main() {',
      '\tfmt.Println("hi", Add(2, 3))',
      '}',
    ].join('\n')
    const lines = tokenizeLines(src, 'go')
    expect(lines.length).toBe(src.split('\n').length)
    for (let i = 0; i < lines.length; i++) {
      expect(lines[i].map((t) => t.text).join('')).toBe(src.split('\n')[i])
    }
  })

  it('keeps a block comment coloured as a comment across lines', () => {
    const src = '/* one\ntwo\nthree */\nfunc F() {}'
    const lines = tokenizeLines(src, 'go')
    expect(lines[0].map((t) => t.kind)).toContain('comment')
    expect(lines[1].every((t) => t.kind === 'comment')).toBe(true)
    expect(lines[2].every((t) => t.kind === 'comment')).toBe(true)
    // The code after the comment is code again.
    expect(lines[3].map((t) => t.kind)).toContain('keyword')
  })

  it('keeps a Go raw string coloured as a string across lines', () => {
    const src = 'var s = `line one\nline two`\nfunc F() {}'
    const lines = tokenizeLines(src, 'go')
    expect(lines[1].every((t) => t.kind === 'string')).toBe(true)
    expect(lines[2].map((t) => t.kind)).toContain('keyword')
  })

  it('colours a call differently from a plain word', () => {
    const lines = tokenizeLines('value := compute(1)', 'go')
    const call = lines[0].find((t) => t.text === 'compute')
    expect(call?.kind).toBe('func')
    // The words around it stay plain: one plain token may hold several words.
    const plain = lines[0].filter((t) => t.kind === 'plain').map((t) => t.text).join('')
    expect(plain).toContain('value')
  })

  it('leaves an unknown language alone', () => {
    const lines = tokenizeLines('func main() {}', 'plain')
    expect(lines[0].every((t) => t.kind === 'plain')).toBe(true)
  })

  it('does not treat a JSON key as code', () => {
    const lines = tokenizeLines('{"name": "svc", "port": 8080}', 'json')
    const byText = new Map(lines[0].map((t) => [t.text, t.kind]))
    expect(byText.get('"name"')).toBe('string')
    expect(byText.get('8080')).toBe('number')
  })
})

describe('LangFromPath', () => {
  it('maps the extensions the project touches', () => {
    expect(LangFromPath('pkg/agent/direct.go')).toBe('go')
    expect(LangFromPath('web/src/store.ts')).toBe('ts')
    expect(LangFromPath('web/src/store.tsx')).toBe('ts')
    expect(LangFromPath('cmd/bench/corpus.py')).toBe('python')
    expect(LangFromPath('a/b.json')).toBe('json')
    expect(LangFromPath('run.sh')).toBe('shell')
    expect(LangFromPath('README.md')).toBe('plain')
  })
})
