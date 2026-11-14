import { useDeferredValue, useEffect, useMemo, useRef, useState } from 'react'
import { ApiError, api } from '../api/client'
import { sameRelPath } from '../lib/conversation'
import { useStore } from '../store'
import type { FileChange, FsFileResponse, TurnChange } from '../types'
import { lineRangeFromOffsets } from '../lib/conversation'
import { draftLocateNotice } from '../lib/search'
import { scrollTopForLine } from '../lib/fileNav'
import { FileDiffList } from './FileDiffView'
import { DocIcon, SpinnerIcon, XIcon } from './icons'
import { LangFromPath, tokenizeLines, type TokenKind } from '../lib/highlight'

// Same rule as the diff view: only the kinds that carry meaning get a colour.
// Plain text inherits the editor's own colour, so an unreadable file stays
// readable.
const TOKEN_CLASS: Record<TokenKind, string> = {
  plain: '',
  keyword: 'text-sky-300',
  type: 'text-teal-300',
  string: 'text-emerald-300',
  number: 'text-amber-300',
  comment: 'text-slate-500 italic',
  func: 'text-brand-300',
}

function humanSize(n: number): string {
  if (n < 1024) return `${n} B`
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`
  return `${(n / 1024 / 1024).toFixed(2)} MB`
}

export function CodeEditor({
  value,
  path,
  onChange,
  readOnly,
  onSelect,
  revealLine,
  revealNonce,
}: {
  value: string
  path: string
  onChange: (next: string) => void
  readOnly?: boolean
  onSelect: (start: number, end: number) => void
  revealLine?: number
  revealNonce?: number
}) {
  const ta = useRef<HTMLTextAreaElement | null>(null)
  const gutter = useRef<HTMLDivElement | null>(null)
  const layer = useRef<HTMLPreElement | null>(null)
  const lineCount = Math.max(1, value.split('\n').length)

  // The highlighted layer sits behind a transparent textarea. It must lag the
  // typing, not block it: tokenizing a big file on every keystroke would make
  // the editor feel sticky.
  const deferred = useDeferredValue(value)
  const tokens = useMemo(() => tokenizeLines(deferred, LangFromPath(path)), [deferred, path])

  function syncScroll() {
    if (gutter.current && ta.current) {
      gutter.current.scrollTop = ta.current.scrollTop
    }
    // The highlighted layer sits behind the textarea, so it has to scroll
    // with it — horizontally too, or a long line drifts out of its column.
    if (layer.current && ta.current) {
      layer.current.scrollTop = ta.current.scrollTop
      layer.current.scrollLeft = ta.current.scrollLeft
    }
  }

  useEffect(() => {
    if (!revealLine || !ta.current) return
    const top = scrollTopForLine(revealLine)
    ta.current.scrollTop = top
    if (gutter.current) gutter.current.scrollTop = top
    if (layer.current) layer.current.scrollTop = top
  }, [revealLine, revealNonce, value])

  return (
    <div className="flex min-h-0 flex-1 overflow-hidden bg-ink-950/60">
      <div
        ref={gutter}
        aria-hidden
        className="w-14 shrink-0 overflow-hidden border-r border-ink-800 bg-ink-950/80 py-2 text-right font-mono text-[12px] leading-[1.6] text-slate-700"
      >
        {Array.from({ length: lineCount }, (_, i) => (
          <div key={i} className={`px-2 ${revealLine === i + 1 ? 'bg-brand-600/30 text-slate-200' : ''}`}>
            {i + 1}
          </div>
        ))}
      </div>
      <div className="relative min-h-0 min-w-0 flex-1">
        <pre
          aria-hidden
          ref={layer}
          className="pointer-events-none absolute inset-0 overflow-hidden whitespace-pre px-3 py-2 font-mono text-[12px] leading-[1.6] text-slate-300"
        >
          {tokens.map((line, i) => (
            <span key={i}>
              {line.map((t, j) => (
                <span key={j} className={TOKEN_CLASS[t.kind]}>
                  {t.text}
                </span>
              ))}
              {'\n'}
            </span>
          ))}
        </pre>
        <textarea
          ref={ta}
          value={value}
          readOnly={readOnly}
          spellCheck={false}
          wrap="off"
          onChange={(e) => onChange(e.target.value)}
          onScroll={syncScroll}
          onSelect={(e) => {
            const el = e.currentTarget
            onSelect(el.selectionStart, el.selectionEnd)
          }}
          className="relative min-h-0 min-w-0 h-full w-full resize-none bg-transparent px-3 py-2 font-mono text-[12px] leading-[1.6] text-transparent caret-slate-300 outline-none"
        />
      </div>
    </div>
  )
}

export function FileViewer({ sessionId, path }: { sessionId: string | null; path: string }) {
  const closeFile = useStore((s) => s.closeFile)
  const currentConvId = useStore((s) => s.currentConvId)
  const preferDiff = useStore((s) => s.fileView?.preferDiff)
  const revealLine = useStore((s) => s.fileView?.line)
  const revealNonce = useStore((s) => s.fileView?.revealNonce)
  const turnChanges = useStore((s) => s.turnChanges)
  const loadTurnChanges = useStore((s) => s.loadTurnChanges)
  const applyTurnHunks = useStore((s) => s.applyTurnHunks)
  const actionError = useStore((s) => s.actionError)
  const setFileDirty = useStore((s) => s.setFileDirty)
  const pendingFileNav = useStore((s) => s.pendingFileNav)
  const confirmFileNav = useStore((s) => s.confirmFileNav)
  const cancelFileNav = useStore((s) => s.cancelFileNav)
  const attachSelection = useStore((s) => s.attachSelection)
  const openAiEditDialog = useStore((s) => s.openAiEditDialog)
  const fileReloadNonce = useStore((s) => s.fileReloadNonce)
  const [file, setFile] = useState<FsFileResponse | null>(null)
  const [draft, setDraft] = useState('')
  const [baseHash, setBaseHash] = useState('')
  const [loading, setLoading] = useState(true)
  const [saving, setSaving] = useState(false)
  const [problem, setProblem] = useState<{ status: number; message: string } | null>(null)
  const [saveError, setSaveError] = useState<string | null>(null)
  const [attachNote, setAttachNote] = useState<string | null>(null)
  const [sel, setSel] = useState<{ start: number; end: number }>({ start: 0, end: 0 })
  const [showDiff, setShowDiff] = useState(!!preferDiff)

  const dirty = !!file && draft !== file.content
  const change: FileChange | undefined = turnChanges?.changes.find((c) => sameRelPath(c.path, path))
  const appliedKey = `${(change as TurnChange | undefined)?.applied_status ?? ''}:${(change as TurnChange | undefined)?.applied_content?.length ?? 0}`

  useEffect(() => {
    setFileDirty(dirty)
    return () => setFileDirty(false)
  }, [dirty, setFileDirty])

  useEffect(() => {
    if (!dirty) return
    const onBefore = (e: BeforeUnloadEvent) => {
      e.preventDefault()
      e.returnValue = ''
    }
    window.addEventListener('beforeunload', onBefore)
    return () => window.removeEventListener('beforeunload', onBefore)
  }, [dirty])

  useEffect(() => {
    if (currentConvId) void loadTurnChanges(turnChanges?.sessionId)
    // Reload when opening a different file; the session id is stable per turn.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [currentConvId, path])

  useEffect(() => {
    if (preferDiff === false) {
      setShowDiff(false)
      return
    }
    if (preferDiff && change) setShowDiff(true)
  }, [preferDiff, change, revealNonce])

  useEffect(() => {
    let cancelled = false
    setLoading(true)
    setProblem(null)
    setSaveError(null)
    setAttachNote(null)
    setFile(null)
    setDraft('')
    setBaseHash('')
    api
      .fsFile(path, sessionId ?? undefined, currentConvId ?? undefined)
      .then((f) => {
        if (cancelled) return
        setFile(f)
        setDraft(f.content)
        setBaseHash(f.hash ?? '')
      })
      .catch((e) => {
        if (cancelled) return
        if (e instanceof ApiError) {
          setProblem({
            status: e.status,
            message:
              e.status === 413
                ? '文件超过 1 MiB 的查看/编辑上限。'
                : e.status === 415
                  ? '二进制文件不支持文本编辑。'
                  : e.status === 403
                    ? '该路径通过软链接指向工作区之外，已被安全策略阻止。'
                    : e.message,
          })
        } else {
          setProblem({ status: 0, message: String(e) })
        }
      })
      .finally(() => {
        if (!cancelled) setLoading(false)
      })
    return () => {
      cancelled = true
    }
  }, [path, sessionId, currentConvId, appliedKey, fileReloadNonce])

  const lines = useMemo(() => {
    if (!file) return []
    const src = showDiff ? file.content : draft
    const out = src.split('\n')
    if (out.length > 1 && out[out.length - 1] === '') out.pop()
    return out
  }, [file, draft, showDiff])

  const canEdit = !!file && file.editable !== false && !file.redacted
  const selected = sel.end !== sel.start

  async function save() {
    if (!file || !canEdit || saving) return
    if (!baseHash) {
      setSaveError('缺少基线版本，请重新加载后再保存。')
      return
    }
    setSaving(true)
    setSaveError(null)
    try {
      const got = await api.fsWrite(path, draft, baseHash, {
        session: sessionId ?? undefined,
        conversation: currentConvId ?? undefined,
      })
      setFile({ ...file, content: draft, size: got.size, hash: got.hash, editable: got.editable ?? true })
      setBaseHash(got.hash)
    } catch (e) {
      setSaveError(e instanceof ApiError ? e.message : String(e))
    } finally {
      setSaving(false)
    }
  }

  function attach() {
    if (!selected || !currentConvId) {
      setAttachNote(currentConvId ? '请先在编辑器中选中一段代码。' : '请先打开一个项目对话，再把选中代码附到需求。')
      return
    }
    const range = lineRangeFromOffsets(draft, sel.start, sel.end)
    const text = draft.slice(Math.min(sel.start, sel.end), Math.max(sel.start, sel.end))
    attachSelection({
      path,
      start_line: range.start_line,
      end_line: range.end_line,
      text,
      draft: dirty,
    })
    setAttachNote(
      dirty
        ? `已附到对话：${path}:${range.start_line}-${range.end_line}（未保存草稿，不会写入项目；发送需求时会带上，并标明这是草稿）。关闭编辑器后可发送。`
        : `已附到对话：${path}:${range.start_line}-${range.end_line}。关闭编辑器后可随下一条需求发送。`,
    )
  }

  function aiEdit() {
    if (!canEdit) return
    if (dirty) {
      setAttachNote('有未保存的修改。AI 改写基于磁盘上的文件，请先保存再改写，免得覆盖你的草稿。')
      return
    }
    if (!currentConvId) {
      setAttachNote('请先打开一个项目对话，再在编辑器里用 AI 改写。')
      return
    }
    const range = lineRangeFromOffsets(draft, sel.start, sel.end)
    if (sel.end !== sel.start) {
      openAiEditDialog(path, { start_line: range.start_line, end_line: range.end_line })
    } else {
      // No selection: Cursor-style "generate here" inserts at the caret line.
      openAiEditDialog(path, { insert_line: range.start_line })
    }
  }

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (!(e.metaKey || e.ctrlKey) || e.key.toLowerCase() !== 'k') return
      if (useStore.getState().aiEditOpen) return
      if (!canEdit) return
      e.preventDefault()
      aiEdit()
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
    // aiEdit closes over the current draft/selection, so rebind when they change.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [canEdit, dirty, currentConvId, draft, sel, path])

  async function reload() {
    setLoading(true)
    setProblem(null)
    setSaveError(null)
    try {
      const f = await api.fsFile(path, sessionId ?? undefined, currentConvId ?? undefined)
      setFile(f)
      setDraft(f.content)
      setBaseHash(f.hash ?? '')
    } catch (e) {
      setSaveError(e instanceof ApiError ? e.message : String(e))
    } finally {
      setLoading(false)
    }
  }

  return (
    <div className="relative flex flex-1 flex-col overflow-hidden">
      <div className="flex shrink-0 flex-wrap items-center gap-2 border-b border-line bg-ink-850 px-4 py-2">
        <DocIcon width={14} height={14} className="text-slate-500" />
        <span className="truncate font-mono text-[12px] text-slate-200" title={path}>
          {path}
        </span>
        {file && (
          <span className="shrink-0 font-mono text-[11px] text-slate-500">
            {humanSize(file.size)} · {lines.length} 行
            {dirty ? ' · 未保存' : canEdit ? ' · 可编辑' : ' · 只读'}
          </span>
        )}
        {change && (
          <div className="ml-2 flex rounded border border-line text-[11px]">
            <button
              type="button"
              className={`px-2 py-0.5 ${!showDiff ? 'bg-ink-800 text-slate-100' : 'text-slate-500 hover:text-slate-300'}`}
              onClick={() => setShowDiff(false)}
            >
              当前文件
            </button>
            <button
              type="button"
              className={`px-2 py-0.5 ${showDiff ? 'bg-ink-800 text-slate-100' : 'text-slate-500 hover:text-slate-300'}`}
              onClick={() => setShowDiff(true)}
            >
              本轮差异
            </button>
          </div>
        )}
        <div className="ml-auto flex shrink-0 items-center gap-1">
          {canEdit && !showDiff && (
            <>
              <button
                type="button"
                disabled={!dirty || saving}
                onClick={() => void save()}
                className="rounded border border-line px-2 py-0.5 text-[11.5px] text-slate-200 hover:bg-ink-800 disabled:cursor-not-allowed disabled:text-slate-600"
              >
                {saving ? '保存中…' : '保存'}
              </button>
              <button
                type="button"
                disabled={dirty}
                onClick={aiEdit}
                title={
                  dirty
                    ? '请先保存，AI 改写基于磁盘上的文件'
                    : '让 AI 改写选中的代码（未选中时按光标位置插入）。⌘/Ctrl+K'
                }
                className="rounded border border-brand-700/60 bg-brand-950/40 px-2 py-0.5 text-[11.5px] text-brand-200 hover:bg-brand-900/50 disabled:cursor-not-allowed disabled:border-line disabled:bg-transparent disabled:text-slate-600"
              >
                AI 改写<span className="ml-1 text-[10px] opacity-70">⌘K</span>
              </button>
              <button
                type="button"
                disabled={!selected}
                onClick={attach}
                title="把当前选中的代码（含路径和行号）附到对话的下一条需求。未保存草稿不会写入项目。"
                className="rounded border border-line px-2 py-0.5 text-[11.5px] text-slate-200 hover:bg-ink-800 disabled:cursor-not-allowed disabled:text-slate-600"
              >
                附到对话
              </button>
            </>
          )}
          {!canEdit && currentConvId && !showDiff && file && (
            <button
              type="button"
              disabled={!selected}
              onClick={attach}
              title="把当前选中的代码附到对话的下一条需求。"
              className="rounded border border-line px-2 py-0.5 text-[11.5px] text-slate-200 hover:bg-ink-800 disabled:cursor-not-allowed disabled:text-slate-600"
            >
              附到对话
            </button>
          )}
          <button
            onClick={closeFile}
            className="flex items-center gap-1 rounded border border-line px-2 py-0.5 text-[11.5px] text-slate-400 hover:bg-ink-800 hover:text-slate-200"
          >
            <XIcon width={12} height={12} />
            关闭
          </button>
        </div>
      </div>

      {file?.reason && (
        <p className="shrink-0 border-b border-line bg-ink-900 px-4 py-1.5 text-[11.5px] text-amber-200/90">{file.reason}</p>
      )}
      {saveError && (
        <div className="flex shrink-0 items-center gap-2 border-b border-line bg-ink-900 px-4 py-1.5 text-[11.5px] text-rose-300">
          <span className="min-w-0 flex-1">{saveError}</span>
          <button type="button" className="underline" onClick={() => void reload()}>
            重新加载
          </button>
        </div>
      )}
      {attachNote && (
        <p className="shrink-0 border-b border-line bg-ink-900 px-4 py-1.5 text-[11.5px] text-sky-200/90">{attachNote}</p>
      )}
      {dirty && revealLine ? (
        <p className="shrink-0 border-b border-line bg-ink-900 px-4 py-1.5 text-[11.5px] text-amber-200/90">
          {draftLocateNotice(path, revealLine)}
        </p>
      ) : null}

      {loading && (
        <div className="min-h-0 flex-1 overflow-auto">
          <p className="flex items-center gap-2 px-5 py-6 text-[12.5px] text-slate-500">
            <SpinnerIcon width={14} height={14} />
            正在读取 {path}…
          </p>
        </div>
      )}

      {problem && !loading && (
        <div className="min-h-0 flex-1 overflow-auto px-5 py-6">
          <p className="text-[13px] text-slate-300">{problem.message}</p>
        </div>
      )}

      {change && showDiff && !loading && (
        <div className="min-h-0 flex-1 overflow-auto px-4 py-3">
          <p className="mb-2 text-[11.5px] text-slate-500">
            下面是这一轮对该文件的改动（来自该轮开始时的快照）。最新一轮且任务已结束后可按块写回工作区；较早轮次只读，以免误改当前文件。撤销整轮修改是另一件事：会把该轮涉及的文件恢复到该轮开始前，不会按块挑选，也不会使用 git reset。
          </p>
          {actionError && <p className="mb-2 text-[12px] text-rose-300">{actionError}</p>}
          <FileDiffList
            changes={[change]}
            editable={!!turnChanges?.editable}
            reason={turnChanges?.reason}
            mode={turnChanges?.editable ? 'apply' : 'readonly'}
            onApply={(payload) => applyTurnHunks(payload)}
          />
        </div>
      )}

      {(!change || !showDiff) && !loading && !problem && file && (
        <CodeEditor
          key={path}
          value={draft}
          path={path}
          readOnly={!canEdit}
          revealLine={revealLine}
          revealNonce={revealNonce}
          onChange={(next) => {
            if (canEdit) setDraft(next)
          }}
          onSelect={(start, end) => setSel({ start, end })}
        />
      )}

      {pendingFileNav && (
        <div className="absolute inset-0 z-20 flex items-center justify-center bg-black/50 p-4">
          <div className="max-w-md rounded-md border border-line bg-ink-900 p-4 text-[13px] text-slate-200 shadow-xl">
            <p className="font-medium">当前文件有未保存的修改</p>
            <p className="mt-2 text-[12.5px] text-slate-400">
              离开不会自动保存，也不会把草稿写进项目。已附到对话的选中内容仍会带上，并标明这是未保存草稿。
            </p>
            <div className="mt-3 flex justify-end gap-2">
              <button type="button" className="btn px-3 py-1 text-[12px]" onClick={cancelFileNav}>
                继续编辑
              </button>
              <button
                type="button"
                className="rounded border border-rose-800/60 bg-rose-950/40 px-3 py-1 text-[12px] text-rose-100"
                onClick={confirmFileNav}
              >
                放弃修改并离开
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  )
}
