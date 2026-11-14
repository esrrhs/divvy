import { useEffect, useMemo, useRef, useState } from 'react'
import { useStore } from '../store'
import type { DiffHunkLine } from '../types'
import { SpinnerIcon, XIcon } from './icons'

const QUICK: { label: string; text: string }[] = [
  { label: '加文档注释', text: '给这段代码加上清晰的文档注释，不要改动逻辑。' },
  { label: '修 bug', text: '这段代码有 bug，请找出并修复，保持对外行为符合注释/函数名。' },
  { label: '简化', text: '在不改变行为的前提下简化这段代码，去掉重复和多余的分支。' },
  { label: '重命名清晰', text: '把这里的命名改得能自解释，变量名/函数名更准确。' },
  { label: '补测试', text: '为这段代码补上表驱动测试，覆盖边界情况。' },
]

function hunkLineClass(type: string): string {
  if (type === 'add') return 'bg-emerald-950/40 text-emerald-200'
  if (type === 'remove') return 'bg-rose-950/40 text-rose-200'
  return 'text-slate-400'
}

function marker(type: string): string {
  if (type === 'add') return '+'
  if (type === 'remove') return '-'
  return ' '
}

// AI inline edit dialog (Cursor Cmd-K style).
//
// The model returns the replacement code for the selected lines. The server
// diffs it and sends hunks, so the user reviews a real diff and can keep only
// the parts they want before anything touches the file.
export function AiEditDialog() {
  const open = useStore((s) => s.aiEditOpen)
  const path = useStore((s) => s.aiEditPath ?? '')
  const sel = useStore((s) => s.aiEditSelection)
  const result = useStore((s) => s.aiEditResult)
  const streaming = useStore((s) => s.aiEditStreaming)
  const busy = useStore((s) => s.aiEditBusy)
  const applied = useStore((s) => s.aiEditApplied)
  const undoDepth = useStore((s) => s.aiEditUndoDepth)
  const history = useStore((s) => s.aiEditHistory)
  const runAiEdit = useStore((s) => s.runAiEdit)
  const resetAiEdit = useStore((s) => s.resetAiEdit)
  const applyAiEdit = useStore((s) => s.applyAiEdit)
  const undoAiEdit = useStore((s) => s.undoAiEdit)
  const closeAiEditDialog = useStore((s) => s.closeAiEditDialog)
  const actionError = useStore((s) => s.actionError)
  const clearActionError = useStore((s) => s.clearActionError)

  const [instruction, setInstruction] = useState('')
  const [rejected, setRejected] = useState<Set<number>>(new Set())
  const streamBox = useRef<HTMLPreElement | null>(null)

  const hunks = result?.hunks ?? []
  const truncated = !!result?.truncated
  const accepted = useMemo(
    () => hunks.filter((h) => !rejected.has(h.index)).length,
    [hunks, rejected],
  )

  useEffect(() => {
    if (open) {
      setInstruction('')
      setRejected(new Set())
    }
  }, [open, path, sel?.start_line, sel?.end_line, sel?.insert_line])

  useEffect(() => {
    if (streamBox.current) streamBox.current.scrollTop = streamBox.current.scrollHeight
  }, [streaming])

  // One entry point for both the button and the keyboard shortcut: the
  // instruction box is cleared only after the model answered, so a failed
  // turn can be retried without retyping it.
  const send = () => {
    const text = instruction.trim()
    if (!text || busy) return
    clearActionError()
    void runAiEdit(text).then((ok) => {
      if (ok) setInstruction('')
    })
  }

  useEffect(() => {
    if (!open) return
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') {
        e.preventDefault()
        closeAiEditDialog()
        return
      }
      const mod = e.metaKey || e.ctrlKey
      if (!mod) return
      if (e.key === 'Enter' && instruction.trim() && !busy) {
        e.preventDefault()
        send()
      } else if (e.key.toLowerCase() === 's' && result && accepted > 0 && !busy) {
        e.preventDefault()
        void applyAiEdit(decisionsFor(hunks, rejected))
      } else if (e.key.toLowerCase() === 'z' && (applied || undoDepth > 0) && !busy) {
        // Cmd-Z undoes the write, the way an editor's does. Inside the
        // instruction box the browser's own text undo wins: hijacking it
        // would eat the user's typing.
        const el = e.target as HTMLElement | null
        const typing = !!el && (el.tagName === 'TEXTAREA' || el.tagName === 'INPUT')
        if (typing) return
        e.preventDefault()
        void undoAiEdit()
      }
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  })

  if (!open) return null

  const rangeText = sel?.insert_line
    ? `在第 ${sel.insert_line} 行前插入`
    : `第 ${sel?.start_line ?? '?'}-${sel?.end_line ?? '?'} 行`

  return (
    // The dialog floats on the right instead of covering the screen: the point
    // of the rewrite is to judge it in the file, and a full-screen modal hides
    // exactly that.
    <div className="fixed inset-0 z-50 flex items-center justify-end p-3 pointer-events-none">
      <div className="pointer-events-auto flex max-h-[92vh] w-full max-w-[540px] flex-col overflow-hidden rounded-xl border border-brand-800/50 bg-ink-850 shadow-2xl">
        <div className="flex items-center gap-2 border-b border-line bg-brand-950/30 px-4 py-2.5">
          <span className={`h-2 w-2 rounded-full ${busy ? 'animate-pulse bg-brand-400' : 'bg-brand-700'}`} />
          <h3 className="text-[13px] font-semibold text-brand-200">AI 改写</h3>
          <span className="ml-auto flex items-center gap-2 font-mono text-[11px] text-slate-500">
            <span className="max-w-[280px] truncate" title={path}>
              {path}
            </span>
            <span className="shrink-0">{rangeText}</span>
            {history.length > 0 && (
              <span className="shrink-0 text-brand-300">第 {history.length} 轮</span>
            )}
          </span>
          <button
            type="button"
            className="rounded p-0.5 text-slate-500 hover:bg-ink-800 hover:text-slate-200"
            onClick={() => closeAiEditDialog()}
            title="关闭（Esc）"
          >
            <XIcon width={13} height={13} />
          </button>
        </div>

        <div className="max-h-[62vh] overflow-auto px-4 py-3">
          {applied || undoDepth > 0 ? (
            <div className="py-3">
              <p className="text-[13px] text-emerald-300">
                {applied
                  ? `已写回 ${applied.applied} 块${applied.rejected > 0 ? `，拒绝 ${applied.rejected} 块` : ''}。`
                  : '已撤销上一次改写。'}
              </p>
              <p className="mt-1.5 text-[12px] text-slate-400">
                文件已从磁盘重新载入。撤销只在这个文件此后没有被其他改动碰过时才生效，否则会拒绝覆盖。
                {undoDepth > 1 && ` ⌘/Ctrl+Z 可连续撤销，还有 ${undoDepth - 1} 次。`}
              </p>
            </div>
          ) : (
            <>
          <div className="mb-3">
            <textarea
              autoFocus
              className="input min-h-[76px] resize-y"
              placeholder={
                result
                  ? '继续说要改什么（例如：再补一个超时分支、把参数校验也加上）'
                  : '例如：给这个函数加文档注释、修掉空指针、把这段循环改成 map、补上边界测试'
              }
              value={instruction}
              onChange={(e) => setInstruction(e.target.value)}
            />
            <div className="mt-1.5 flex flex-wrap items-center gap-1.5">
              {QUICK.map((q) => (
                <button
                  key={q.label}
                  type="button"
                  className="rounded border border-line px-1.5 py-0.5 text-[11px] text-slate-400 hover:bg-ink-800 hover:text-slate-200"
                  onClick={() => setInstruction(q.text)}
                >
                  {q.label}
                </button>
              ))}
              <span className="ml-auto text-[11px] text-slate-500">⌘/Ctrl+↵ 生成 · ⌘/Ctrl+S 应用 · Esc 关闭</span>
            </div>
          </div>

          <div className="mb-3 flex items-center justify-between gap-2">
            <button
              type="button"
              disabled={busy || !instruction.trim()}
              className="btn-primary px-3 py-1.5 text-[12px]"
              onClick={send}
            >
              {busy && <SpinnerIcon width={13} height={13} />}
              {busy ? '生成中…' : result ? '继续改' : '生成候选'}
            </button>
            <div className="flex items-center gap-3">
              {history.length > 0 && (
                <button
                  type="button"
                  className="text-[11px] text-slate-500 underline decoration-dotted hover:text-slate-300"
                  title="丢掉前面的对话，从原选区重新开始"
                  onClick={() => {
                    resetAiEdit()
                    setInstruction('')
                  }}
                >
                  从头再来
                </button>
              )}
              {result && hunks.length > 0 && (
                <span className="text-[11px] text-slate-400">
                  将应用 {accepted} / {hunks.length} 块
                  {rejected.size > 0 && ` · 已拒绝 ${rejected.size} 块`}
                </span>
              )}
            </div>
          </div>

          {actionError && (
            <p className="mb-3 rounded border border-rose-800/60 bg-rose-950/40 px-2.5 py-1.5 text-[12px] text-rose-300">
              {actionError}
            </p>
          )}

          {busy && !result && (
            <div className="overflow-hidden rounded-md border border-line bg-ink-950/50">
              <div className="flex items-center gap-2 border-b border-line bg-ink-900 px-3 py-1.5 text-[11px] text-slate-500">
                <SpinnerIcon width={12} height={12} />
                模型输出（实时）
                {truncated ? ' · 文件较大，只发送了选区附近的窗口' : ''}
              </div>
              <pre
                ref={streamBox}
                className="max-h-[240px] overflow-auto whitespace-pre-wrap break-all p-3 font-mono text-[11.5px] leading-relaxed text-slate-300"
              >
                {streaming || '等待模型返回…'}
              </pre>
            </div>
          )}

          {!busy && !result && !actionError && (
            <p className="text-[12px] text-slate-500">
              写清楚要改什么，模型只重写选中的这几行，不会碰文件的其他部分。生成后先看 diff，再决定应用哪些块。
            </p>
          )}

          {result && (result.context_before || result.context_after || result.new_text) && (
            <div className="mb-3 overflow-hidden rounded-md border border-line bg-ink-950/50">
              <div className="border-b border-line bg-ink-900 px-3 py-1.5 text-[11px] text-slate-500">
                改动所在的位置（上下文）
              </div>
              <pre className="max-h-[220px] overflow-auto whitespace-pre-wrap break-all p-3 font-mono text-[11.5px] leading-[1.55] text-slate-500">
                {result.context_before}
                <span className="bg-emerald-950/50 text-emerald-200">{result.new_text}</span>
                {result.context_after}
              </pre>
            </div>
          )}

          {result && hunks.length === 0 && (
            <p className="rounded border border-line bg-ink-900 px-2.5 py-1.5 text-[12px] text-slate-400">
              模型返回的代码与原选区一致，没有可应用的改动。
            </p>
          )}

          {result && hunks.length > 0 && (
            <div className="overflow-hidden rounded-md border border-line">
              <div className="flex items-center justify-between border-b border-line bg-ink-900 px-3 py-1.5 text-[11px] text-slate-400">
                <span className="font-mono">候选 diff（相对文件原文，点块标题可整块拒绝 / 接受）</span>
                <span className="flex gap-2">
                  <button
                    type="button"
                    className="underline decoration-dotted hover:text-slate-200"
                    onClick={() => setRejected(new Set())}
                  >
                    全接受
                  </button>
                  <button
                    type="button"
                    className="underline decoration-dotted hover:text-slate-200"
                    onClick={() => setRejected(new Set(hunks.map((h) => h.index)))}
                  >
                    全拒绝
                  </button>
                </span>
              </div>
              <div className="max-h-[300px] overflow-auto font-mono text-[11.5px] leading-relaxed">
                {hunks.map((h) => {
                  const off = rejected.has(h.index)
                  return (
                    <div key={h.index} className={off ? 'opacity-40' : ''}>
                      <button
                        type="button"
                        className={`flex w-full items-center gap-2 border-y border-line/60 px-3 py-1 text-left text-[11px] ${
                          off ? 'bg-ink-900 text-rose-300' : 'bg-brand-950/40 text-brand-200'
                        }`}
                        onClick={() =>
                          setRejected((prev) => {
                            const next = new Set(prev)
                            if (next.has(h.index)) next.delete(h.index)
                            else next.add(h.index)
                            return next
                          })
                        }
                      >
                        <span>@@ 块 {h.index + 1} @@</span>
                        <span className="ml-auto">{off ? '已拒绝（点击恢复）' : '接受中（点击拒绝）'}</span>
                      </button>
                      {h.lines.map((l: DiffHunkLine, i) => (
                        <div key={i} className={`flex gap-2 px-3 ${hunkLineClass(l.type)}`}>
                          <span className="w-8 shrink-0 select-none text-right text-slate-600">
                            {l.old_no ?? l.new_no ?? ''}
                          </span>
                          <span className="w-2 shrink-0 select-none">{marker(l.type)}</span>
                          <span className="whitespace-pre-wrap break-all">{l.text || ' '}</span>
                        </div>
                      ))}
                    </div>
                  )
                })}
              </div>
            </div>
          )}
            </>
          )}
        </div>

        <div className="flex items-center justify-end gap-2 border-t border-line bg-ink-900/50 px-4 py-2.5">
          {applied || undoDepth > 0 ? (
            <>
              <button
                type="button"
                disabled={busy}
                className="rounded border border-rose-800/60 bg-rose-950/30 px-3 py-1.5 text-[12px] text-rose-200 hover:bg-rose-950/50 disabled:text-rose-900"
                onClick={() => void undoAiEdit()}
                title="把文件恢复到这次改写之前。文件此后被其他改动碰过时会拒绝。"
              >
                {busy && <SpinnerIcon width={13} height={13} />}
                {undoDepth > 1 ? `再撤销上一次（还有 ${undoDepth - 1} 次）` : '撤销这次改写'}
              </button>
              <button type="button" className="btn-primary px-3 py-1.5 text-[12px]" onClick={() => closeAiEditDialog()}>
                完成
              </button>
            </>
          ) : (
            <>
              <button type="button" className="btn-ghost px-3 py-1.5 text-[12px]" onClick={() => closeAiEditDialog()}>
                取消
              </button>
              {result && hunks.length > 0 && (
                <button
                  type="button"
                  disabled={busy || accepted === 0}
                  className="btn-primary px-3 py-1.5 text-[12px]"
                  onClick={() => void applyAiEdit(decisionsFor(hunks, rejected))}
                >
                  {busy && <SpinnerIcon width={13} height={13} />}
                  应用 {accepted} 块
                </button>
              )}
            </>
          )}
        </div>
      </div>
    </div>
  )
}

function decisionsFor(hunks: { index: number }[], rejected: Set<number>): boolean[] {
  return hunks.map((h) => !rejected.has(h.index))
}
