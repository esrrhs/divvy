import { useEffect, useMemo, useRef, useState } from 'react'
import { useStore } from '../store'
import type { AiEditProposal, AiEditResponse, AiEditTarget, AiEditTurn, DiffHunkLine } from '../types'
import { clampDialogPos } from '../lib/conversation'
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

// The multi-turn transcript: what the user asked each round and what the
// model produced, per file with its apply state. Follow-up rounds ride on
// this context server-side (buildAiEditPrompt pastes the prior turns), so
// showing it here keeps the inline-edit conversation legible.
function AiEditHistoryList({ turns }: { turns: AiEditTurn[] }) {
  const [openIdx, setOpenIdx] = useState<number | null>(null)
  return (
    <div className="mb-3 rounded border border-line bg-ink-900/60">
      <div className="flex items-center gap-2 border-b border-line px-2.5 py-1 text-[10.5px] uppercase tracking-wider text-slate-600">
        多轮上下文
        <span className="ml-auto normal-case text-slate-600">后续轮会带着这些指令与产出</span>
      </div>
      {turns.map((t, i) => {
        const files =
          t.files && t.files.length > 0
            ? t.files
            : t.code
              ? [{ path: t.path || '', code: t.code, applied: t.applied }]
              : []
        const open = openIdx === i
        return (
          <div key={i} className="border-b border-line/60 last:border-b-0">
            <button
              type="button"
              className="flex w-full items-center gap-2 px-2.5 py-1.5 text-left hover:bg-ink-800/60"
              onClick={() => setOpenIdx(open ? null : i)}
              title={open ? '收起这一轮的产出' : '展开这一轮的产出'}
            >
              <span className="shrink-0 font-mono text-[10px] text-brand-300">第 {i + 1} 轮</span>
              <span className="min-w-0 flex-1 truncate text-[11.5px] text-slate-300">{t.instruction}</span>
              <span className="shrink-0 text-[10px] text-slate-600">{open ? '收起' : '展开'}</span>
            </button>
            {open && (
              <div className="px-2.5 pb-2">
                {files.map((f, j) => (
                  <div key={j} className="mt-1 overflow-hidden rounded border border-line/60 bg-ink-950/70">
                    <div className="flex items-center gap-2 px-2 py-0.5 font-mono text-[10px] text-slate-500">
                      <span className="truncate">{f.path || '（当前文件）'}</span>
                      <span className={`ml-auto shrink-0 ${f.applied ? 'text-emerald-400' : 'text-amber-400/80'}`}>
                        {f.applied ? '已应用' : '待应用'}
                      </span>
                    </div>
                    <pre className="max-h-40 overflow-auto whitespace-pre px-2 pb-1.5 font-mono text-[10.5px] leading-[1.5] text-slate-400">
                      {f.code}
                    </pre>
                  </div>
                ))}
              </div>
            )}
          </div>
        )
      })}
    </div>
  )
}

function marker(type: string): string {
  if (type === 'add') return '+'
  if (type === 'remove') return '-'
  return ' '
}

function shortPath(path: string): string {
  const parts = path.split(/[\\/]/)
  return parts.slice(-2).join('/')
}

function proposalsOf(result: AiEditResponse | null): AiEditProposal[] {
  if (!result) return []
  return result.proposals?.length ? result.proposals : [result]
}

function rangeText(t: AiEditTarget): string {
  if (t.selection.insert_line) return `第 ${t.selection.insert_line} 行前插入`
  return `第 ${t.selection.start_line ?? '?'}-${t.selection.end_line ?? '?'} 行`
}

// AI inline edit dialog (Cursor Cmd-K style).
//
// The model returns the replacement code for ONE selected range. A round can
// have several files: the server makes one code-only call per file, so a weak
// model never has to emit a JSON list of edits. The dialog keeps one decision
// set per file and the write-back sends them together as one atomic step.
export function AiEditDialog() {
  const open = useStore((s) => s.aiEditOpen)
  const path = useStore((s) => s.aiEditPath ?? '')
  const sel = useStore((s) => s.aiEditSelection)
  const targetsStore = useStore((s) => s.aiEditTargets)
  const result = useStore((s) => s.aiEditResult)
  const streaming = useStore((s) => s.aiEditStreaming)
  const progress = useStore((s) => s.aiEditProgress)
  const busy = useStore((s) => s.aiEditBusy)
  const applied = useStore((s) => s.aiEditApplied)
  const undoDepth = useStore((s) => s.aiEditUndoDepth)
  const history = useStore((s) => s.aiEditHistory)
  const runAiEdit = useStore((s) => s.runAiEdit)
  const resetAiEdit = useStore((s) => s.resetAiEdit)
  const applyAiEdit = useStore((s) => s.applyAiEdit)
  const undoAiEdit = useStore((s) => s.undoAiEdit)
  const closeAiEditDialog = useStore((s) => s.closeAiEditDialog)
  const removeAiEditTarget = useStore((s) => s.removeAiEditTarget)
  const actionError = useStore((s) => s.actionError)
  const clearActionError = useStore((s) => s.clearActionError)

  const [instruction, setInstruction] = useState('')
  // Decisions are per FILE: rejecting a hunk in a.go must not reject the
  // same-numbered hunk in b.go.
  const [rejected, setRejected] = useState<Record<string, Set<number>>>({})
  const [activePath, setActivePath] = useState('')

  const targets: AiEditTarget[] = useMemo(
    () => (targetsStore.length ? targetsStore : path && sel ? [{ path, selection: sel }] : []),
    [targetsStore, path, sel],
  )
  const proposals = useMemo(() => proposalsOf(result), [result])
  const multi = targets.length > 1
  const chosenPath = activePath || targets[0]?.path || ''
  const active = proposals.find((p) => p.path === chosenPath) ?? proposals[0] ?? null
  const isTruncated = !!active?.truncated || !!result?.truncated
  const hunks = active?.hunks ?? []
  const rejectedHere = active ? rejected[active.path] ?? new Set<number>() : new Set<number>()
  const accepted = hunks.filter((h) => !rejectedHere.has(h.index)).length

  const toggleHunk = (index: number) => {
    if (!active) return
    setRejected((prev) => {
      const next = { ...prev }
      const set = new Set(next[active.path] ?? [])
      if (set.has(index)) set.delete(index)
      else set.add(index)
      next[active.path] = set
      return next
    })
  }

  const decisionsMap = () => {
    const out: Record<string, boolean[]> = {}
    for (const p of proposals) {
      const set = rejected[p.path] ?? new Set<number>()
      out[p.path] = (p.hunks ?? []).map((h) => !set.has(h.index))
    }
    return out
  }
  const totalAccepted = proposals.reduce(
    (n, p) => n + (p.hunks ?? []).filter((h) => !(rejected[p.path] ?? new Set<number>()).has(h.index)).length,
    0,
  )

  useEffect(() => {
    if (open) {
      setInstruction('')
      setRejected({})
      setActivePath('')
    }
  }, [open, path, sel?.start_line, sel?.end_line, sel?.insert_line])

  // Changing the target set invalidates the proposals: they were generated
  // for the old list of files.
  const targetKey = targets.map((t) => t.path).join('|')
  useEffect(() => {
    setRejected({})
    setActivePath('')
  }, [targetKey])

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
      } else if (e.key.toLowerCase() === 's' && result && totalAccepted > 0 && !busy) {
        e.preventDefault()
        void applyAiEdit(decisionsMap())
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

  // Free position (offsets from the viewport's top/right edges). null = the
  // docked default (right-centered). Persisted so the chosen spot survives a
  // reload; the header drag handle and double-click manage it.
  const [pos, setPos] = useState<{ top: number; right: number } | null>(() => {
    try {
      const raw = JSON.parse(localStorage.getItem('divvy.aiEditPos') ?? 'null') as {
        top?: number
        right?: number
      } | null
      if (!raw || typeof raw.top !== 'number' || typeof raw.right !== 'number') return null
      return clampDialogPos(raw.top, raw.right, 540, 300, window.innerWidth, window.innerHeight)
    } catch {
      return null
    }
  })
  const panelRef = useRef<HTMLDivElement | null>(null)
  const dragStart = useRef<{ x: number; y: number; top: number; right: number } | null>(null)

  function startDrag(e: React.MouseEvent) {
    if (e.button !== 0) return
    e.preventDefault()
    const rect = panelRef.current?.getBoundingClientRect()
    if (!rect) return
    const base = pos ?? { top: rect.top, right: window.innerWidth - rect.right }
    dragStart.current = { x: e.clientX, y: e.clientY, top: base.top, right: base.right }
    const onMove = (ev: MouseEvent) => {
      const st = dragStart.current
      if (!st) return
      const next = clampDialogPos(
        st.top + (ev.clientY - st.y),
        st.right - (ev.clientX - st.x),
        rect.width,
        rect.height,
        window.innerWidth,
        window.innerHeight,
      )
      setPos(next)
    }
    const onUp = () => {
      window.removeEventListener('mousemove', onMove)
      window.removeEventListener('mouseup', onUp)
      setPos((p) => {
        try {
          localStorage.setItem('divvy.aiEditPos', JSON.stringify(p))
        } catch {
          // Private-mode storage: the position just won't persist.
        }
        return p
      })
    }
    window.addEventListener('mousemove', onMove)
    window.addEventListener('mouseup', onUp)
  }

  function resetPos() {
    try {
      localStorage.removeItem('divvy.aiEditPos')
    } catch {
      // Some environments ship a localStorage shim without removeItem;
      // an empty value reads back as garbage and is treated as docked.
      try {
        localStorage.setItem('divvy.aiEditPos', '')
      } catch {
        // Best effort either way.
      }
    }
    setPos(null)
  }

  if (!open) return null

  const headerRange = multi
    ? `${targets.length} 个文件`
    : sel?.insert_line
      ? `在第 ${sel.insert_line} 行前插入`
      : `第 ${sel?.start_line ?? '?'}-${sel?.end_line ?? '?'} 行`

  const applyLabel = multi
    ? `应用 ${totalAccepted} 块（${proposals.filter((p) => (p.hunks ?? []).some((h) => !(rejected[p.path] ?? new Set<number>()).has(h.index))).length} 个文件）`
    : `应用 ${accepted} 块`

  return (
    // The dialog floats on the right instead of covering the screen: the point
    // of the rewrite is to judge it in the file, and a full-screen modal hides
    // exactly that.
    <div
      className={`fixed inset-0 z-50 pointer-events-none ${pos ? '' : 'flex items-center justify-end p-3'}`}
    >
      <div
        ref={panelRef}
        className="pointer-events-auto flex max-h-[92vh] w-full max-w-[540px] flex-col overflow-hidden rounded-xl border border-brand-800/50 bg-ink-850 shadow-2xl"
        style={pos ? { position: 'absolute', top: pos.top, right: pos.right } : undefined}
      >
        <div
          className="flex cursor-move select-none items-center gap-2 border-b border-line bg-brand-950/30 px-4 py-2.5"
          onMouseDown={startDrag}
          onDoubleClick={resetPos}
          title="拖动移动对话框（不遮挡查看器工具条）；双击回到停靠位置"
        >
          <span className={`h-2 w-2 rounded-full ${busy ? 'animate-pulse bg-brand-400' : 'bg-brand-700'}`} />
          <h3 className="text-[13px] font-semibold text-brand-200">AI 改写</h3>
          <span className="ml-auto flex items-center gap-2 font-mono text-[11px] text-slate-500">
            {!multi && (
              <span className="max-w-[280px] truncate" title={path}>
                {path}
              </span>
            )}
            <span className="shrink-0">{headerRange}</span>
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
          {applied ? (
            <div className="py-3">
              <p className="text-[13px] text-emerald-300">
                已写回 {applied.applied} 块{applied.rejected > 0 ? `，拒绝 ${applied.rejected} 块` : ''}。
              </p>
              {applied.paths && applied.paths.length > 1 && (
                <p className="mt-1 font-mono text-[11.5px] text-slate-400">
                  {applied.paths.join('、')} 作为一次写回，撤销会一起退回。
                </p>
              )}
              <p className="mt-1.5 text-[12px] text-slate-400">
                文件已从磁盘重新载入。撤销只在这个文件此后没有被其他改动碰过时才生效，否则会拒绝覆盖。
                {undoDepth > 1 && ` ⌘/Ctrl+Z 可连续撤销，还有 ${undoDepth - 1} 次。`}
              </p>
            </div>
          ) : (
            <>
              {undoDepth > 0 && (
                <p className="mb-3 rounded border border-line bg-ink-900 px-2.5 py-1.5 text-[11.5px] text-slate-400">
                  上一次改写仍可撤销（还有 {undoDepth} 次），⌘/Ctrl+Z 继续退回。
                </p>
              )}

              {history.length > 0 && <AiEditHistoryList turns={history} />}

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

              {multi && (
                <div className="mb-3">
                  <div className="mb-1 flex items-center justify-between text-[11px] text-slate-500">
                    <span>这一轮会改这些文件（点切换审阅）</span>
                    <span>在别的文件里选中代码按 ⌘K 可加入</span>
                  </div>
                  <div className="flex flex-wrap gap-1.5">
                    {targets.map((t) => {
                      const p = proposals.find((x) => x.path === t.path)
                      const isActive = t.path === chosenPath
                      return (
                        <span
                          key={t.path}
                          className={`inline-flex items-center gap-1 rounded border px-1.5 py-1 font-mono text-[11px] ${
                            isActive ? 'border-brand-700/70 bg-brand-950/40 text-brand-200' : 'border-line text-slate-400'
                          }`}
                        >
                          <button
                            type="button"
                            title={t.path}
                            onClick={() => {
                              setActivePath(t.path)
                              useStore.getState().openFile(t.path)
                            }}
                          >
                            {shortPath(t.path)}:{rangeText(t)}
                          </button>
                          {p?.error && <span className="text-rose-300" title={p.error}>失败</span>}
                          {targets.length > 1 && (
                            <button
                              type="button"
                              className="text-slate-500 hover:text-rose-300"
                              title="从这一轮移除"
                              onClick={() => removeAiEditTarget(t.path)}
                            >
                              ×
                            </button>
                          )}
                        </span>
                      )
                    })}
                  </div>
                </div>
              )}

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
                      {rejectedHere.size > 0 && ` · 已拒绝 ${rejectedHere.size} 块`}
                    </span>
                  )}
                </div>
              </div>

              {actionError && (
                <p className="mb-3 rounded border border-rose-800/60 bg-rose-950/40 px-2.5 py-1.5 text-[12px] text-rose-300">
                  {actionError}
                </p>
              )}
              {active?.error && (
                <p className="mb-3 rounded border border-rose-800/60 bg-rose-950/40 px-2.5 py-1.5 text-[12px] text-rose-300">
                  {active.path} 生成失败：{active.error}。这个文件不会被写回，其它文件仍可应用。
                </p>
              )}

              {busy && !result && (
                <div className="overflow-hidden rounded-md border border-line bg-ink-950/50">
                  <div className="flex items-center gap-2 border-b border-line bg-ink-900 px-3 py-1.5 text-[11px] text-slate-500">
                    <SpinnerIcon width={12} height={12} />
                    {multi ? (progress || `正在生成 ${targets.length} 个文件…`) : '模型输出（实时）'}
                    {!multi && isTruncated ? ' · 文件较大，只发送了选区附近的窗口' : ''}
                  </div>
                  <pre className="max-h-[240px] overflow-auto whitespace-pre-wrap break-all p-3 font-mono text-[11.5px] leading-relaxed text-slate-300">
                    {multi ? (progress || '等待模型返回…') : streaming || '等待模型返回…'}
                  </pre>
                </div>
              )}

              {!busy && !result && !actionError && (
                <p className="text-[12px] text-slate-500">
                  写清楚要改什么，模型只重写选中的这几行，不会碰文件的其他部分。生成后先看 diff，再决定应用哪些块。
                  {!multi && ' 想一起改别的文件，去那个文件里选中代码再按 ⌘K，会加进同一轮。'}
                </p>
              )}

              {result && active && (active.context_before || active.context_after || active.new_text) && !active.error && (
                <div className="mb-3 overflow-hidden rounded-md border border-line bg-ink-950/50">
                  <div className="border-b border-line bg-ink-900 px-3 py-1.5 text-[11px] text-slate-500">
                    改动所在的位置（上下文）{multi && <span className="ml-1 font-mono">{shortPath(active.path)}</span>}
                  </div>
                  <pre className="max-h-[220px] overflow-auto whitespace-pre-wrap break-all p-3 font-mono text-[11.5px] leading-[1.55] text-slate-500">
                    {active.context_before}
                    <span className="bg-emerald-950/50 text-emerald-200">{active.new_text}</span>
                    {active.context_after}
                  </pre>
                </div>
              )}

              {result && active && hunks.length === 0 && !active.error && (
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
                        onClick={() =>
                          setRejected((prev) => ({ ...prev, [active!.path]: new Set() }))
                        }
                      >
                        全接受
                      </button>
                      <button
                        type="button"
                        className="underline decoration-dotted hover:text-slate-200"
                        onClick={() =>
                          setRejected((prev) => ({ ...prev, [active!.path]: new Set(hunks.map((h) => h.index)) }))
                        }
                      >
                        全拒绝
                      </button>
                    </span>
                  </div>
                  <div className="max-h-[300px] overflow-auto font-mono text-[11.5px] leading-relaxed">
                    {hunks.map((h) => {
                      const off = rejectedHere.has(h.index)
                      return (
                        <div key={h.index} className={off ? 'opacity-40' : ''}>
                          <button
                            type="button"
                            className={`flex w-full items-center gap-2 border-y border-line/60 px-3 py-1 text-left text-[11px] ${
                              off ? 'bg-ink-900 text-rose-300' : 'bg-brand-950/40 text-brand-200'
                            }`}
                            onClick={() => toggleHunk(h.index)}
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
          {applied ? (
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
              {undoDepth > 0 && (
                <button
                  type="button"
                  disabled={busy}
                  className="text-[11px] text-slate-400 underline decoration-dotted hover:text-slate-200"
                  onClick={() => void undoAiEdit()}
                >
                  撤销上一次（还有 {undoDepth} 次）
                </button>
              )}
              {result && proposals.some((p) => (p.hunks ?? []).length > 0) && (
                <button
                  type="button"
                  disabled={busy || totalAccepted === 0}
                  className="btn-primary px-3 py-1.5 text-[12px]"
                  onClick={() => void applyAiEdit(decisionsMap())}
                >
                  {busy && <SpinnerIcon width={13} height={13} />}
                  {applyLabel}
                </button>
              )}
            </>
          )}
        </div>
      </div>
    </div>
  )
}
