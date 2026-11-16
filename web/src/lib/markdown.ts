// A minimal markdown block/inline parser for model replies in the
// conversation. Zero dependencies on purpose: the engine ships no UI package
// registry, and the subset a coding assistant emits is small and stable —
// fenced code blocks, headings, bullet lists, blockquotes, bold and inline
// code. Anything unrecognized renders as plain paragraphs, which is always a
// safe fallback for free-form model output.

export type InlineSpan =
  | { t: 'text'; s: string }
  | { t: 'code'; s: string }
  | { t: 'bold'; s: string }

export type MarkdownBlock =
  | { kind: 'code'; lang: string; code: string }
  | { kind: 'p'; spans: InlineSpan[] }
  | { kind: 'h'; level: number; spans: InlineSpan[] }
  | { kind: 'li'; spans: InlineSpan[] }
  | { kind: 'quote'; spans: InlineSpan[] }
  | { kind: 'hr' }

interface PendingPar {
  kind: 'p'
  lines: string[]
}

type Pending = PendingPar | Extract<MarkdownBlock, { kind: 'code' }>

/** Parse model-reply text into renderable blocks. Never throws. */
export function parseMarkdown(text: string): MarkdownBlock[] {
  const blocks: MarkdownBlock[] = []
  const pending: Pending[] = []

  const flushPar = () => {
    const top = pending[0]
    if (top && top.kind === 'p') {
      if (top.lines.length > 0) blocks.push({ kind: 'p', spans: parseInline(top.lines.join('\n')) })
      pending.length = 0
    }
  }
  const pushPar = (line: string) => {
    const top = pending[0]
    if (top && top.kind === 'p') {
      top.lines.push(line)
      return
    }
    flushPar()
    pending.push({ kind: 'p', lines: [line] })
  }

  const lines = (text ?? '').split('\n')
  let inCode = false
  let codeLang = ''
  let codeLines: string[] = []

  for (const raw of lines) {
    if (inCode) {
      const close = /^\s*```/.test(raw)
      if (close) {
        inCode = false
        flushPar()
        blocks.push({ kind: 'code', lang: codeLang, code: codeLines.join('\n') })
        codeLines = []
        codeLang = ''
        continue
      }
      codeLines.push(raw)
      continue
    }
    const fence = raw.match(/^\s*```(\s*([\w+#.-]+))?\s*$/)
    if (fence) {
      flushPar()
      inCode = true
      codeLang = (fence[2] ?? '').toLowerCase()
      continue
    }
    const h = raw.match(/^(#{1,6})\s+(.*)$/)
    if (h) {
      flushPar()
      blocks.push({ kind: 'h', level: h[1].length, spans: parseInline(h[2]) })
      continue
    }
    if (/^\s*(-{3,}|\*{3,}|_{3,})\s*$/.test(raw)) {
      flushPar()
      blocks.push({ kind: 'hr' })
      continue
    }
    const li = raw.match(/^\s*(?:[-*+]|\d+[.)])\s+(.*)$/)
    if (li) {
      flushPar()
      blocks.push({ kind: 'li', spans: parseInline(li[1]) })
      continue
    }
    const q = raw.match(/^\s*>\s?(.*)$/)
    if (q) {
      flushPar()
      blocks.push({ kind: 'quote', spans: parseInline(q[1]) })
      continue
    }
    if (raw.trim() === '') {
      flushPar()
      continue
    }
    pushPar(raw)
  }
  // An unclosed fence still renders as code: a truncated model reply must not
  // turn its tail into garbled paragraphs.
  if (inCode) {
    flushPar()
    blocks.push({ kind: 'code', lang: codeLang, code: codeLines.join('\n') })
  }
  flushPar()
  return blocks
}

/** Parse `code` and **bold** spans; everything else stays plain text. */
export function parseInline(text: string): InlineSpan[] {
  const spans: InlineSpan[] = []
  const re = /`([^`]+)`|\*\*([^*]+)\*\*/g
  let last = 0
  let m: RegExpExecArray | null
  while ((m = re.exec(text))) {
    if (m.index > last) spans.push({ t: 'text', s: text.slice(last, m.index) })
    if (m[1] !== undefined) spans.push({ t: 'code', s: m[1] })
    else spans.push({ t: 'bold', s: m[2] ?? '' })
    last = m.index + m[0].length
  }
  if (last < text.length) spans.push({ t: 'text', s: text.slice(last) })
  return spans.length > 0 ? spans : [{ t: 'text', s: text }]
}

/** Map a fenced-code language tag to a filename the highlighter understands. */
export function langToFileExt(lang: string): string {
  const l = (lang ?? '').trim().toLowerCase()
  const map: Record<string, string> = {
    go: '.go',
    golang: '.go',
    ts: '.ts',
    typescript: '.ts',
    tsx: '.tsx',
    js: '.js',
    javascript: '.js',
    jsx: '.jsx',
    py: '.py',
    python: '.py',
    rs: '.rs',
    rust: '.rs',
    sh: '.sh',
    bash: '.sh',
    shell: '.sh',
    zsh: '.sh',
    json: '.json',
    yaml: '.yaml',
    yml: '.yaml',
    toml: '.toml',
    sql: '.sql',
    html: '.html',
    css: '.css',
    md: '.md',
    c: '.c',
    cpp: '.cpp',
    'c++': '.cpp',
    java: '.java',
    rb: '.rb',
    php: '.php',
    kt: '.kt',
    swift: '.swift',
    diff: '.diff',
  }
  return map[l] ?? '.txt'
}
