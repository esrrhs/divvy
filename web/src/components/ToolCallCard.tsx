import { useState } from 'react'
import type { ToolActivity } from '../types'
import { copyToClipboard } from '../lib/conversation'
import { ChevronRightIcon } from './icons'

// What 复制 puts on the clipboard: the output as recorded (head, gap, tail)
// — the most useful text when pasting into a report or a follow-up prompt.
// A tool with no output copies the args instead.
function cardCopyText(t: ToolActivity): string {
  const out = [t.output_head, t.output_tail].filter(Boolean).join('\n⋯\n')
  if (out) return out
  return t.args ?? ''
}

// formatDuration keeps the header compact: sub-second calls read as ms,
// everything else as seconds with one decimal (a 5-minute build is still
// one token wide).
export function formatDuration(ms?: number): string {
  if (ms == null || !Number.isFinite(ms) || ms < 0) return ''
  if (ms < 1000) return `${Math.round(ms)}ms`
  return `${(ms / 1000).toFixed(ms < 10000 ? 1 : 0)}s`
}

export function formatOutputLen(len?: number): string {
  if (len == null || !Number.isFinite(len) || len <= 0) return ''
  if (len < 1024) return `${len} B`
  return `${(len / 1024).toFixed(1)} kB`
}

// Whether the recorded output lost its middle: only then does the expanded
// card show a gap separator between head and tail.
function hasGap(t: ToolActivity): boolean {
  if (!t.output_tail) return false
  const head = t.output_head?.length ?? 0
  const tail = t.output_tail.length
  return (t.output_len ?? head + tail) > head + tail
}

// ToolCallCard renders one finished tool call the way the agent loop saw
// it: status, duration, and expandable args/output. The flat text line the
// server also stores stays the fallback for messages recorded before the
// structured field existed.
export function ToolCallCard({ tool }: { tool: ToolActivity }) {
  const [open, setOpen] = useState(false)
  const [copied, setCopied] = useState(false)
  const failed = !tool.ok
  const dur = formatDuration(tool.duration_ms)
  const len = formatOutputLen(tool.output_len)

  async function copy() {
    if (await copyToClipboard(cardCopyText(tool))) {
      setCopied(true)
      setTimeout(() => setCopied(false), 1200)
    }
  }

  return (
    <div
      className={`rounded-md border ${failed ? 'border-rose-900/60 bg-rose-950/20' : 'border-line bg-ink-900/40'} px-3 py-2`}
    >
      <button
        type="button"
        onClick={() => setOpen((o) => !o)}
        className="flex w-full items-center gap-2 text-left"
        title={open ? '收起工具详情' : '展开工具详情'}
      >
        <ChevronRightIcon
          className={`h-3.5 w-3.5 shrink-0 text-slate-500 transition-transform ${open ? 'rotate-90' : ''}`}
        />
        <span className={`h-2 w-2 shrink-0 rounded-full ${failed ? 'bg-rose-500' : 'bg-emerald-500'}`} />
        <span className="truncate font-mono text-[12px] text-slate-200">{tool.tool}</span>
        {failed && <span className="text-[10.5px] font-medium text-rose-400">失败</span>}
        <span className="ml-auto flex shrink-0 items-center gap-2 font-mono text-[10px] text-slate-600">
          {len && <span>{len}</span>}
          {dur && <span>{dur}</span>}
        </span>
      </button>
      {open && (
        <div className="mt-2 space-y-2 border-t border-line pt-2">
          <div className="flex justify-end">
            <button
              type="button"
              className="text-[10px] text-slate-600 hover:text-brand-200"
              title="复制工具输出（无输出时复制参数）"
              onClick={() => void copy()}
            >
              {copied ? '已复制' : '复制输出'}
            </button>
          </div>
          {tool.error && (
            <p className="whitespace-pre-wrap break-words font-mono text-[11.5px] leading-relaxed text-rose-300">
              {tool.error}
            </p>
          )}
          {tool.args && (
            <div>
              <p className="text-[10px] font-medium uppercase tracking-wider text-slate-600">参数</p>
              <pre className="mt-1 overflow-x-auto whitespace-pre-wrap break-words font-mono text-[11px] leading-relaxed text-slate-400">
                {tool.args}
              </pre>
            </div>
          )}
          {(tool.output_head || tool.output_tail) && (
            <div>
              <p className="text-[10px] font-medium uppercase tracking-wider text-slate-600">输出</p>
              <pre className="mt-1 max-h-60 overflow-y-auto whitespace-pre-wrap break-words font-mono text-[11px] leading-relaxed text-slate-300">
                {tool.output_head}
                {hasGap(tool) && (
                  <span className="my-1 block text-center text-slate-600">
                    ⋯ 中间 {Math.max((tool.output_len ?? 0) - (tool.output_head?.length ?? 0) - (tool.output_tail?.length ?? 0), 0)} 字符已省略 ⋯
                  </span>
                )}
                {tool.output_tail}
              </pre>
            </div>
          )}
          {!tool.error && !tool.args && !tool.output_head && !tool.output_tail && (
            <p className="text-[11.5px] text-slate-600">（无参数与输出记录）</p>
          )}
        </div>
      )}
    </div>
  )
}
