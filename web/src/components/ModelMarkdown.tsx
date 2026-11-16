import { useMemo } from 'react'
import { langToFileExt, parseMarkdown, type InlineSpan, type MarkdownBlock } from '../lib/markdown'
import { LangFromPath, tokenizeLines, type TokenKind } from '../lib/highlight'

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

function CodeBlock({ lang, code }: { lang: string; code: string }) {
  const lines = useMemo(() => code.split('\n'), [code])
  const lang_ = useMemo(() => LangFromPath('x' + langToFileExt(lang)), [lang])
  const tokens = useMemo(() => tokenizeLines(code, lang_), [code, lang_])
  return (
    <div className="mt-1 overflow-hidden rounded border border-line bg-ink-950/80">
      {lang !== '' && (
        <div className="border-b border-line bg-ink-900/60 px-2.5 py-0.5 font-mono text-[10px] uppercase tracking-wide text-slate-500">
          {lang}
        </div>
      )}
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
    </div>
  )
}

function Block({ b }: { b: MarkdownBlock }) {
  switch (b.kind) {
    case 'code':
      return <CodeBlock lang={b.lang} code={b.code} />
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
 * degrades to plain text, never throws. */
export function ModelMarkdown({ text }: { text: string }) {
  const blocks = useMemo(() => parseMarkdown(text), [text])
  return (
    <div className="mt-1 min-w-0">
      {blocks.map((b, i) => (
        <Block key={i} b={b} />
      ))}
    </div>
  )
}
