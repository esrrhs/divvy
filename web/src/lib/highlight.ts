// A small, dependency-free syntax highlighter.
//
// Why hand-rolled: the web bundle deliberately carries three runtime
// dependencies (react, react-dom, zustand). A highlighter library would add a
// grammar tree we would only use for four languages, and the project's rule
// is that every dependency has to earn its place.
//
// What it is NOT: a parser. It is a line-oriented tokenizer that knows the
// keywords, the string forms, the comments and the numbers of the languages
// this project actually touches. A mis-tokenized line is a wrong color, never
// a wrong text — the code shown is always the code on disk.

export type TokenKind =
  | 'plain'
  | 'keyword'
  | 'type'
  | 'string'
  | 'number'
  | 'comment'
  | 'func'

export interface Token {
  text: string
  kind: TokenKind
}

/** The languages the tokenizer knows. Anything else stays plain. */
export type Lang = 'go' | 'ts' | 'python' | 'json' | 'shell' | 'plain'

const GO_KEYWORDS = new Set([
  'break', 'case', 'chan', 'const', 'continue', 'default', 'defer', 'else',
  'fallthrough', 'for', 'func', 'go', 'goto', 'if', 'import', 'interface',
  'map', 'package', 'range', 'return', 'select', 'struct', 'switch', 'type', 'var',
])

const GO_TYPES = new Set([
  'bool', 'byte', 'complex64', 'complex128', 'error', 'float32', 'float64',
  'int', 'int8', 'int16', 'int32', 'int64', 'rune', 'string', 'uint', 'uint8',
  'uint16', 'uint32', 'uint64', 'uintptr', 'any',
])

const TS_KEYWORDS = new Set([
  'abstract', 'as', 'async', 'await', 'break', 'case', 'catch', 'class', 'const',
  'continue', 'declare', 'default', 'delete', 'do', 'else', 'enum', 'export',
  'extends', 'false', 'finally', 'for', 'from', 'function', 'get', 'if',
  'implements', 'import', 'in', 'instanceof', 'interface', 'let', 'namespace',
  'new', 'null', 'of', 'private', 'protected', 'public', 'readonly', 'return',
  'set', 'static', 'super', 'switch', 'this', 'throw', 'true', 'try', 'type',
  'typeof', 'undefined', 'var', 'void', 'while', 'with', 'yield',
])

const TS_TYPES = new Set([
  'any', 'Array', 'boolean', 'bigint', 'Date', 'Map', 'never', 'number',
  'object', 'Promise', 'Record', 'Set', 'string', 'symbol', 'unknown', 'void',
])

const PY_KEYWORDS = new Set([
  'and', 'as', 'assert', 'async', 'await', 'break', 'class', 'continue', 'def',
  'del', 'elif', 'else', 'except', 'False', 'finally', 'for', 'from', 'global',
  'if', 'import', 'in', 'is', 'lambda', 'None', 'nonlocal', 'not', 'or', 'pass',
  'raise', 'return', 'True', 'try', 'while', 'with', 'yield',
])

const PY_TYPES = new Set([
  'bool', 'bytes', 'dict', 'float', 'int', 'list', 'object', 'set', 'str',
  'tuple',
])

const SHELL_KEYWORDS = new Set([
  'case', 'do', 'done', 'elif', 'else', 'esac', 'export', 'fi', 'for', 'function',
  'if', 'in', 'local', 'return', 'then', 'until', 'while',
])

interface LangSpec {
  keywords: Set<string>
  types: Set<string>
  /** Comment starters that run to the end of the line. */
  lineComments: string[]
  /** Block comment delimiters, if the language has them. */
  blockComment?: [string, string]
  /** Quote characters that delimit a string. */
  quotes: string[]
  /** Backtick strings (Go raw strings) may span lines. */
  rawBacktick?: boolean
  /** JSON has no comments and no keywords worth colouring. */
  bare?: boolean
  /** The plain language gets no colouring at all. */
  noTokens?: boolean
}

const SPECS: Record<Lang, LangSpec> = {
  go: {
    keywords: GO_KEYWORDS,
    types: GO_TYPES,
    lineComments: ['//'],
    blockComment: ['/*', '*/'],
    quotes: ['"', "'"],
    rawBacktick: true,
  },
  ts: {
    keywords: TS_KEYWORDS,
    types: TS_TYPES,
    lineComments: ['//'],
    blockComment: ['/*', '*/'],
    quotes: ['"', "'", '`'],
  },
  python: {
    keywords: PY_KEYWORDS,
    types: PY_TYPES,
    lineComments: ['#'],
    quotes: ['"', "'"],
  },
  json: {
    keywords: new Set(['true', 'false', 'null']),
    types: new Set(),
    lineComments: [],
    quotes: ['"'],
    bare: true,
  },
  shell: {
    keywords: SHELL_KEYWORDS,
    types: new Set(),
    lineComments: ['#'],
    quotes: ['"', "'"],
  },
  plain: {
    keywords: new Set(),
    types: new Set(),
    lineComments: [],
    quotes: [],
    noTokens: true,
  },
}

/** LangFromPath maps a file path to a language the tokenizer knows. */
export function LangFromPath(path: string): Lang {
  const p = path.toLowerCase()
  if (p.endsWith('.go')) return 'go'
  if (p.endsWith('.ts') || p.endsWith('.tsx') || p.endsWith('.js') || p.endsWith('.jsx') || p.endsWith('.mjs') || p.endsWith('.cjs')) {
    return 'ts'
  }
  if (p.endsWith('.py')) return 'python'
  if (p.endsWith('.json')) return 'json'
  if (p.endsWith('.sh') || p.endsWith('.bash') || p.endsWith('.zsh')) return 'shell'
  return 'plain'
}

function isIdentStart(c: string): boolean {
  return /[A-Za-z_$À-￿]/.test(c)
}

function isIdentPart(c: string): boolean {
  return /[A-Za-z0-9_$À-￿]/.test(c)
}

function isDigit(c: string): boolean {
  return c >= '0' && c <= '9'
}

/**
 * tokenizeLines splits source into one token array per line.
 *
 * The block-comment and raw-string state carries across lines: a comment that
 * opens on one line and closes three lines later must not have its tail
 * coloured as code.
 */
export function tokenizeLines(src: string, lang: Lang): Token[][] {
  const spec = SPECS[lang] ?? SPECS.plain
  const out: Token[][] = []
  let inBlock = false
  let inRaw = false

  const lines = src.split('\n')
  for (const line of lines) {
    const tokens: Token[] = []
    let i = 0
    let plain = ''

    const flush = () => {
      if (plain) {
        tokens.push({ text: plain, kind: 'plain' })
        plain = ''
      }
    }
    const push = (text: string, kind: TokenKind) => {
      flush()
      tokens.push({ text, kind })
    }

    while (i < line.length) {
      // Inside a block comment or a raw string: consume until it ends.
      if (inRaw) {
        const end = line.indexOf('`')
        if (end < 0) {
          push(line.slice(i), 'string')
          i = line.length
          break
        }
        push(line.slice(i, end + 1), 'string')
        i = end + 1
        inRaw = false
        continue
      }
      if (inBlock) {
        const end = line.indexOf('*/')
        if (end < 0) {
          push(line.slice(i), 'comment')
          i = line.length
          break
        }
        push(line.slice(i, end + 2), 'comment')
        i = end + 2
        inBlock = false
        continue
      }

      const rest = line.slice(i)

      // A line comment ends the line.
      const lineComment = spec.lineComments.find((c) => rest.startsWith(c))
      if (lineComment) {
        push(rest, 'comment')
        i = line.length
        break
      }
      // A block comment opens.
      if (spec.blockComment && rest.startsWith(spec.blockComment[0])) {
        const end = rest.indexOf(spec.blockComment[1], spec.blockComment[0].length)
        if (end < 0) {
          push(rest, 'comment')
          i = line.length
          inBlock = true
          break
        }
        push(rest.slice(0, end + spec.blockComment[1].length), 'comment')
        i += end + spec.blockComment[1].length
        continue
      }
      // A string.
      const quote = spec.quotes.find((q) => rest.startsWith(q))
      if (quote) {
        let j = i + quote.length
        while (j < line.length) {
          if (line[j] === '\\') {
            j += 2
            continue
          }
          if (line[j] === quote) {
            j++
            break
          }
          j++
        }
        push(line.slice(i, Math.min(j, line.length)), 'string')
        i = Math.min(j, line.length)
        continue
      }
      // A Go raw string may span lines.
      if (spec.rawBacktick && rest.startsWith('`')) {
        const end = line.indexOf('`', i + 1)
        if (end < 0) {
          push(line.slice(i), 'string')
          i = line.length
          inRaw = true
          break
        }
        push(line.slice(i, end + 1), 'string')
        i = end + 1
        continue
      }
      // A number.
      if (isDigit(rest[0]) || (rest[0] === '.' && isDigit(rest[1] ?? ''))) {
        let j = i
        while (j < line.length && /[0-9a-fA-FxX._]/.test(line[j])) j++
        push(line.slice(i, j), 'number')
        i = j
        continue
      }
      // An identifier: keyword, type, function call, or plain text.
      if (isIdentStart(rest[0])) {
        let j = i
        while (j < line.length && isIdentPart(line[j])) j++
        const word = line.slice(i, j)
        let kind: TokenKind = 'plain'
        if (!spec.bare && spec.keywords.has(word)) kind = 'keyword'
        else if (!spec.bare && spec.types.has(word)) kind = 'type'
        else if (!spec.noTokens) {
          // A call: the identifier is followed by '('.
          const after = line.slice(j).match(/^\s*\(/)
          if (after) kind = 'func'
        }
        if (kind === 'plain') plain += word
        else push(word, kind)
        i = j
        continue
      }
      plain += rest[0]
      i++
    }
    flush()
    out.push(tokens)
  }
  return out
}
