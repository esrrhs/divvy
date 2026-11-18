import { useEffect, useMemo, useState } from 'react'
import { fencePathHint, langToFileExt, parseMarkdown, type InlineSpan, type MarkdownBlock } from '../lib/markdown'
import { LangFromPath, tokenizeLines, type TokenKind } from '../lib/highlight'
import type { FileRef } from '../types'

// Token colours shared with the file viewer and diff views — code blocks in
// model replies must read exactly like the same code in a file.
const TOKEN_CLASS: Record<TokenKind, string> = {
  plain: '',
  keyword: 'text-sky-300',
  type: 'text-teal-300',
  string: 'text-emerald-300',
  number: 'text-amber-300',
  comment: 'text-slate-500 italic',
  func: 'text-brand-300',
}

// What a code block needs to offer "apply to a file": the conversation-scoped
// file search and a confined whole-file write. write returns null on success
// or the error message; the caller owns toasts/refresh.
export interface CodeBlockActions {
  search: (q: string) => Promise<FileRef[]>
  write: (path: string, content: string) => Promise<string | null>
}

function Inline({ spans }: { spans: InlineSpan[] }) {
  return (
    <>
      {spans.map((sp, i) => {
        if (sp.t === 'code') {
          return (
            <code key={i} className="rounded bg-ink-800/80 px-1 py-px font-mono text-[11.5px] text-brand-200">
              {sp.s}
            </code>
          )
        }
        if (sp.t === 'bold') {
          return (
            <strong key={i} className="font-semibold text-slate-100">
              {sp.s}
            </strong>
          )
        }
        return <span key={i}>{sp.s}</span>
      })}
    </>
  )
}

function CodeBlock({
  lang,
  meta,
  code,
  actions,
}: {
  lang: string
  meta?: string
  code: string
  actions?: CodeBlockActions
}) {
  const lines = useMemo(() => code.split('\n'), [code])
  const lang_ = useMemo(() => LangFromPath('x' + langToFileExt(lang)), [lang])
  const tokens = useMemo(() => tokenizeLines(code, lang_), [code, lang_])
  const hint = useMemo(() => fencePathHint(meta ?? ''), [meta])
  const [pickOpen, setPickOpen] = useState(false)
  const [query, setQuery] = useState('')
  const [results, setResults] = useState<FileRef[] | null>(null)
  const [searching, setSearching] = useState(false)
  const [target, setTarget] = useState<string | null>(null)
  const [writing, setWriting] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [donePath, setDonePath] = useState<string | null>(null)
  const [copied, setCopied] = useState(false)

  // The picker searches while open (debounced): the fence's path hint seeds
  // the query, so "```go math/math.go" lands on the right file immediately.
  useEffect(() => {
    if (!pickOpen || !actions) return
    let cancelled = false
    setSearching(true)
    const t = setTimeout(() => {
      actions
        .search(query)
        .then((r) => {
          if (!cancelled) {
            setResults(r)
            setSearching(false)
          }
        })
        .catch(() => {
          if (!cancelled) {
            setResults([])
            setSearching(false)
          }
        })
    }, 200)
    return () => {
      cancelled = true
      clearTimeout(t)
    }
  }, [pickOpen, query, actions])

  async function copy() {
    try {
      await navigator.clipboard.writeText(code)
      setCopied(true)
      setTimeout(() => setCopied(false), 1200)
    } catch {
      // Clipboard unavailable (permissions/jsdom): the button is best effort.
    }
  }

  function openPicker() {
    setDonePath(null)
    setError(null)
    setTarget(null)
    setQuery(hint)
    setPickOpen((o) => !o)
  }

  async function apply() {
    if (!target || !actions) return
    setWriting(true)
    setError(null)
    const err = await actions.write(target, code)
    setWriting(false)
    if (err) {
      setError(err)
      return
    }
    setDonePath(target)
    setPickOpen(false)
    setTarget(null)
  }

  const queryPathLike = /[\\/]/.test(query) || /\.[A-Za-z0-9]+$/.test(query.trim())

  return (
    <div className="mt-1 overflow-hidden rounded border border-line bg-ink-950/80">
      <div className="flex items-center gap-2 border-b border-line bg-ink-900/60 px-2.5 py-0.5 font-mono text-[10px] uppercase tracking-wide text-slate-500">
        {lang !== '' && <span>{lang}</span>}
        {hint && <span className="normal-case text-slate-400">{hint}</span>}
        {actions && (
          <span className="ml-auto flex items-center gap-1.5">
            <button
              type="button"
              className="text-[10px] normal-case text-slate-500 hover:text-brand-200"
              onClick={() => void copy()}
            >
              {copied ? '已复制' : '复制'}
            </button>
            <button
              type="button"
              className="text-[10px] normal-case text-slate-500 hover:text-brand-200"
              onClick={openPicker}
            >
              {donePath ? `已写入 ${donePath}` : '应用到文件…'}
            </button>
          </span>
        )}
      </div>
      <pre className="overflow-auto whitespace-pre px-2.5 py-1.5 font-mono text-[11.5px] leading-[1.55]">
        {lines.map((_, i) => (
          <span key={i} className="block">
            {tokens[i].map((t, j) => (
              <span key={j} className={TOKEN_CLASS[t.kind]}>
                {t.text}
              </span>
            ))}
            {'\n'}
          </span>
        ))}
      </pre>
      {pickOpen && actions && (
        <div className="border-t border-line bg-ink-900/80 px-2.5 py-2">
          {target ? (
            <div className="flex flex-wrap items-center gap-2 text-[11px]">
              <span className="text-slate-300">
                将把代码块整体写入 <span className="font-mono text-brand-200">{target}</span>
                （已存在的文件会被覆盖；写入有防外部修改校验）
              </span>
              <button
                type="button"
                className="btn-primary px-2 py-0.5 text-[11px]"
                disabled={writing}
                onClick={() => void apply()}
              >
                {writing ? '正在写入…' : '写入'}
              </button>
              <button type="button" className="btn px-2 py-0.5 text-[11px]" onClick={() => setTarget(null)}>
                取消
              </button>
            </div>
          ) : (
            <>
              <input
                className="input w-full py-1 font-mono text-[11px]"
                placeholder="搜索目标文件，或输入新路径（写入会整体覆盖已有文件）"
                value={query}
                onChange={(e) => setQuery(e.target.value)}
              />
              {searching && <p className="py-1 text-[11px] text-slate-500">搜索中…</p>}
              {!searching && results && (
                <div className="mt-1 max-h-32 overflow-y-auto">
                  {results.map((r) => (
                    <button
                      key={r.path}
                      type="button"
                      className="block w-full truncate rounded px-1.5 py-0.5 text-left font-mono text-[11px] text-slate-300 hover:bg-ink-800"
                      onClick={() => setTarget(r.path)}
                    >
                      {r.path}
                    </button>
                  ))}
                  {results.length === 0 && !queryPathLike && (
                    <p className="py-1 text-[11px] text-slate-600">没有匹配文件；输入带扩展名的路径可创建新文件</p>
                  )}
                </div>
              )}
              {queryPathLike && !results?.some((r) => r.path === query.trim()) && (
                <button
                  type="button"
                  className="mt-1 rounded border border-line px-2 py-0.5 font-mono text-[11px] text-slate-300 hover:bg-ink-800"
                  onClick={() => setTarget(query.trim())}
                >
                  写入新文件 {query.trim()}
                </button>
              )}
            </>
          )}
          {error && <p className="mt-1 text-[11px] text-rose-300">{error}</p>}
        </div>
      )}
    </div>
  )
}

function Block({
  b,
  actions,
}: {
  b: MarkdownBlock
  actions?: CodeBlockActions
}) {
  switch (b.kind) {
    case 'code':
      return <CodeBlock lang={b.lang} meta={b.meta} code={b.code} actions={actions} />
    case 'h': {
      const size = b.level <= 2 ? 'text-[14px]' : b.level === 3 ? 'text-[13px]' : 'text-[12.5px]'
      return (
        <p className={`mt-2 mb-0.5 font-semibold text-slate-100 ${size}`}>
          <Inline spans={b.spans} />
        </p>
      )
    }
    case 'li':
      return (
        <p className="ml-3 flex gap-1.5 text-[12.5px] leading-relaxed">
          <span aria-hidden className="select-none text-slate-600">
            •
          </span>
          <span className="min-w-0">
            <Inline spans={b.spans} />
          </span>
        </p>
      )
    case 'quote':
      return (
        <blockquote className="ml-2 border-l-2 border-line pl-2.5 text-[12.5px] leading-relaxed text-slate-400">
          <Inline spans={b.spans} />
        </blockquote>
      )
    case 'hr':
      return <div className="my-1.5 border-t border-line" />
    default:
      return (
        <p className="whitespace-pre-wrap break-words text-[12.5px] leading-relaxed">
          <Inline spans={b.spans} />
        </p>
      )
  }
}

/** Model replies render as markdown: fenced code blocks highlighted, inline
 * code/bold styled, everything else as paragraphs. Free-form output always
 * degrades to plain text, never throws. With codeActions, fenced blocks offer
 * copy/apply-to-file like a chat-first editor. */
export function ModelMarkdown({
  text,
  codeActions,
}: {
  text: string
  codeActions?: CodeBlockActions
}) {
  const blocks = useMemo(() => parseMarkdown(text), [text])
  return (
    <div className="mt-1 min-w-0">
      {blocks.map((b, i) => (
        <Block key={i} b={b} actions={codeActions} />
      ))}
    </div>
  )
}
