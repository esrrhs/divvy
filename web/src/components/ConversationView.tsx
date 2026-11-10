import { useEffect, useRef, useState, type FormEvent, type KeyboardEvent } from 'react'
import { api } from '../api/client'
import {
  composeState,
  mentionQuery,
  messageKindMeta,
  outcomeMeta,
  selectionLabel,
  shortWorkdir,
  steerPendingText,
} from '../lib/conversation'
import { useStore } from '../store'
import type { CodeSelection, ConvMessage, FileRef, MessageMode, TurnChange, TurnChangesResponse } from '../types'
import { FileDiffList } from './FileDiffView'
import { SpinnerIcon } from './icons'

function refLabel(r: FileRef): string {
  if (r.type === 'selection' || r.start_line) {
    return selectionLabel({
      path: r.path,
      start_line: r.start_line || 1,
      end_line: r.end_line || r.start_line || 1,
      draft: r.draft,
    })
  }
  return r.path
}

function AttachmentChips({ refs, onRemove }: { refs: FileRef[]; onRemove?: (index: number) => void }) {
  if (!refs.length) return null
  return (
    <div className="mt-1.5 flex flex-wrap gap-1">
      {refs.map((r, i) => (
        <span
          key={`${r.type}-${r.path}-${r.start_line ?? ''}-${r.end_line ?? ''}-${i}`}
          className="inline-flex items-center gap-1 rounded border border-line bg-ink-900 px-1.5 py-0.5 font-mono text-[10.5px] text-slate-300"
          title={r.excluded ? `${refLabel(r)}（未纳入：${r.excluded}）` : r.draft ? `${refLabel(r)}：未保存草稿，未写入项目` : refLabel(r)}
        >
          {r.type === 'dir' ? '📁' : r.type === 'selection' ? '✂️' : '📄'} {refLabel(r)}
          {r.draft && <span className="text-amber-300">草稿</span>}
          {r.excluded && <span className="text-amber-400">跳过</span>}
          {r.truncated && <span className="text-slate-500">截断</span>}
          {onRemove && (
            <button type="button" className="text-slate-500 hover:text-slate-200" onClick={() => onRemove(i)}>
              ×
            </button>
          )}
        </span>
      ))}
    </div>
  )
}

function SelectionChips({
  items,
  onRemove,
}: {
  items: CodeSelection[]
  onRemove?: (index: number) => void
}) {
  if (!items.length) return null
  return (
    <div className="mb-1.5">
      <p className="text-[11px] text-slate-500">将随下一条需求发送的代码选区（不会自动把草稿写入项目）</p>
      <AttachmentChips
        refs={items.map((s) => ({
          path: s.path,
          type: 'selection',
          start_line: s.start_line,
          end_line: s.end_line,
          draft: s.draft,
        }))}
        onRemove={onRemove}
      />
    </div>
  )
}

function ResultCard({ msg, convId }: { msg: ConvMessage; convId: string }) {
  const outcome = outcomeMeta(msg.outcome)
  const applyTurnHunks = useStore((s) => s.applyTurnHunks)
  const loadTurnChanges = useStore((s) => s.loadTurnChanges)
  const storeTurn = useStore((s) => s.turnChanges)
  const [open, setOpen] = useState(false)
  const [loading, setLoading] = useState(false)
  const [changes, setChanges] = useState<TurnChange[] | null>(null)
  const [editable, setEditable] = useState(false)
  const [reason, setReason] = useState('')
  const [error, setError] = useState<string | null>(null)
  const [busyPath, setBusyPath] = useState('')
  const rollbackTurn = useStore((s) => s.rollbackTurn)
  const conversation = useStore((s) => s.conversation)
  const [confirmRollback, setConfirmRollback] = useState(false)
  const [rolling, setRolling] = useState(false)
  const summaries = msg.changes ?? []
  const alreadyRolled = !!(msg.session_id && conversation?.rolled_back_turns?.includes(msg.session_id))
  const phase = conversation?.run_phase
  const pendingRollback = conversation?.pending_rollback
  const rollbackBlocked =
    !!pendingRollback ||
    !!(phase && ['planning', 'plan_review', 'running', 'leaf_approval', 'ask', 'paused'].includes(phase))

  function adopt(got: TurnChangesResponse | { changes?: TurnChange[]; editable?: boolean; reason?: string }) {
    setChanges(got.changes ?? [])
    setEditable(!!got.editable)
    setReason(got.reason ?? '')
  }

  async function toggle() {
    if (open) {
      setOpen(false)
      return
    }
    setOpen(true)
    if (changes || !msg.session_id) return
    setLoading(true)
    setError(null)
    try {
      const got = await api.getTurnChanges(convId, msg.session_id)
      adopt(got)
      if (useStore.getState().turnChanges?.sessionId === msg.session_id) {
        await loadTurnChanges(msg.session_id)
      }
    } catch (e) {
      setError(e instanceof Error ? e.message : '无法加载差异')
    } finally {
      setLoading(false)
    }
  }

  async function onApply(payload: { path: string; all?: string; decisions?: { index: number; accept: boolean }[] }) {
    setBusyPath(payload.path)
    setError(null)
    try {
      const ok = await applyTurnHunks(payload, msg.session_id)
      if (!ok) {
        setError(useStore.getState().actionError || '写回工作区失败')
        return
      }
      const latest = useStore.getState().turnChanges
      if (latest && latest.sessionId === msg.session_id) {
        setChanges(latest.changes)
        setEditable(!!latest.editable)
        setReason(latest.reason ?? '')
      } else if (msg.session_id) {
        const got = await api.getTurnChanges(convId, msg.session_id)
        adopt(got)
      }
    } finally {
      setBusyPath('')
    }
  }

  return (
    <div className="rounded-md border border-line bg-ink-900/70 px-3 py-2">
      <div className="flex items-center gap-2">
        <span className="text-[10.5px] font-medium uppercase tracking-wider text-slate-500">执行结果</span>
        <span className={`rounded border px-1.5 py-px text-[10.5px] ${outcome.cls}`}>{outcome.label}</span>
        {summaries.length > 0 ? (
          <span className="text-[11px] text-slate-400">改了 {summaries.length} 个文件</span>
        ) : (
          <span className="text-[11px] text-slate-600">本轮没有文件改动</span>
        )}
        {msg.session_id && (
          <span className="ml-auto truncate font-mono text-[10px] text-slate-700">{msg.session_id}</span>
        )}
      </div>
      {msg.text && <p className="mt-1 whitespace-pre-wrap break-words text-[12.5px] leading-relaxed text-slate-300">{msg.text}</p>}
      {summaries.length > 0 && (
        <ul className="mt-1.5 space-y-0.5 font-mono text-[11px] text-slate-400">
          {summaries.map((c) => (
            <li key={c.path}>
              <span className="text-slate-500">{c.status}</span>{' '}
              <button
                type="button"
                className="text-slate-200 underline decoration-slate-600 underline-offset-2 hover:text-white"
                onClick={() => useStore.getState().openFile(c.path, msg.session_id, { preferDiff: true })}
              >
                {c.path}
              </button>
              {c.binary ? '（二进制）' : ''}
              {c.truncated ? '（已截断）' : ''}
            </li>
          ))}
        </ul>
      )}
      {msg.session_id && (
        <div className="mt-2 flex flex-wrap items-center gap-2">
          <button type="button" className="btn px-2 py-1 text-[11px]" onClick={() => void toggle()}>
            {open ? '收起差异' : '查看并按块审阅'}
          </button>
          {summaries.length > 0 && !alreadyRolled && (
            <button
              type="button"
              disabled={rolling || rollbackBlocked}
              title={
                pendingRollback
                  ? pendingRollback.message || '上次撤销未完成，请先恢复工作区后再试'
                  : rollbackBlocked
                    ? '当前轮还在执行、审批或暂停，不能撤销已有轮次的文件'
                    : '确认后只恢复这一轮涉及的文件。若工作区已被后续轮次或外部改过，会停止且不覆盖。不会执行 git reset，也不会改无关文件。多文件撤销有操作日志和备份，不是瞬间同时提交。'
              }
              className="rounded border border-line px-2 py-1 text-[11px] text-slate-300 hover:bg-ink-800 disabled:cursor-not-allowed disabled:text-slate-600"
              onClick={() => setConfirmRollback(true)}
            >
              撤销这轮修改
            </button>
          )}
          {alreadyRolled && <span className="text-[11px] text-slate-500">这一轮的修改已撤销</span>}
        </div>
      )}
      {confirmRollback && (
        <div className="mt-2 rounded border border-amber-800/50 bg-amber-950/20 px-3 py-2 text-[12px] text-amber-100">
          <p>
            确认撤销这一轮对工作区的修改？只会动该轮涉及的文件，恢复到该轮开始前的内容（新增文件会删除）。按块接受/拒绝是写回当前工作区的手工挑选；这次是整轮回退。若有文件已被后续轮次、外部编辑或按块写回改过，会停止且不覆盖。不会执行 git reset hard。撤销后会在对话里留下说明，后续模型不会再把这些改动当成仍然有效。较早轮次的快照仍可只读回看。
          </p>
          <div className="mt-2 flex gap-2">
            <button
              type="button"
              disabled={rolling}
              className="rounded border border-rose-800/60 bg-rose-950/40 px-2 py-1 text-[11px] text-rose-100"
              onClick={async () => {
                if (!msg.session_id) return
                setRolling(true)
                setError(null)
                const ok = await rollbackTurn(msg.session_id)
                setRolling(false)
                if (ok) {
                  setConfirmRollback(false)
                } else {
                  setError(useStore.getState().actionError || '撤销失败')
                }
              }}
            >
              {rolling ? '正在撤销…' : '确认撤销'}
            </button>
            <button type="button" className="btn px-2 py-1 text-[11px]" onClick={() => setConfirmRollback(false)}>
              取消
            </button>
          </div>
        </div>
      )}
      {open && (
        <div className="mt-2">
          {loading && <p className="text-[12px] text-slate-500">正在加载本轮差异…</p>}
          {error && <p className="text-[12px] text-rose-300">{error}</p>}
          {changes && (
            <FileDiffList
              changes={storeTurn && storeTurn.sessionId === msg.session_id ? storeTurn.changes : changes}
              editable={storeTurn && storeTurn.sessionId === msg.session_id ? !!storeTurn.editable : editable}
              reason={storeTurn && storeTurn.sessionId === msg.session_id ? storeTurn.reason : reason}
              mode={
                (storeTurn && storeTurn.sessionId === msg.session_id ? storeTurn.editable : editable)
                  ? 'apply'
                  : 'readonly'
              }
              onApply={onApply}
              busyPath={busyPath}
            />
          )}
        </div>
      )}
    </div>
  )
}

function MessageBubble({ msg, convId }: { msg: ConvMessage; convId: string }) {
  if (msg.kind === 'run_finished') {
    return <ResultCard msg={msg} convId={convId} />
  }
  const meta = messageKindMeta(msg.kind)
  return (
    <div className={`rounded-md border px-3 py-2 ${meta.cls}`}>
      <div className="flex items-baseline gap-2">
        <span className="text-[10.5px] font-medium uppercase tracking-wider text-slate-500">{meta.label}</span>
        {msg.session_id && (
          <span className="ml-auto truncate font-mono text-[10px] text-slate-700">{msg.session_id}</span>
        )}
      </div>
      <p className="mt-1 whitespace-pre-wrap break-words text-[12.5px] leading-relaxed">{msg.text}</p>
      {msg.attachments && <AttachmentChips refs={msg.attachments} />}
    </div>
  )
}

export function ConversationView() {
  const conv = useStore((s) => s.conversation)
  const postMessage = useStore((s) => s.postMessage)
  const pendingSelections = useStore((s) => s.pendingSelections)
  const removeSelection = useStore((s) => s.removeSelection)
  const resumeConversation = useStore((s) => s.resumeConversation)
  const cancelQueue = useStore((s) => s.cancelQueue)
  const cancelSteer = useStore((s) => s.cancelSteer)
  const setConversationKey = useStore((s) => s.setConversationKey)
  const answer = useStore((s) => s.answer)
  const pause = useStore((s) => s.pause)
  const live = useStore((s) => s.live)
  const actionError = useStore((s) => s.actionError)
  const recoverRollback = useStore((s) => s.recoverRollback)
  const [text, setText] = useState('')
  const [recovering, setRecovering] = useState(false)
  const [askText, setAskText] = useState('')
  const [askBusy, setAskBusy] = useState(false)
  const [apiKey, setApiKey] = useState('')
  const [busy, setBusy] = useState(false)
  const [paths, setPaths] = useState<FileRef[]>([])
  const [picker, setPicker] = useState<{ start: number; query: string; items: FileRef[] } | null>(null)
  const [highlight, setHighlight] = useState(0)
  const scroller = useRef<HTMLDivElement>(null)
  const area = useRef<HTMLTextAreaElement>(null)
  const searchGen = useRef(0)
  // Per-conversation, per-operation tokens. Visible busy/ask/recover flags
  // are aggregated from this table so one dialogue's completion cannot clear
  // another request's lock, including after a switch or unmount.
  type ComposerOp = 'send' | 'saveKey' | 'ask' | 'recover'
  const inflightOps = useRef(new Map<string, Map<ComposerOp, number>>())
  const opToken = useRef(0)
  const mountedRef = useRef(true)

  useEffect(() => {
    mountedRef.current = true
    return () => {
      mountedRef.current = false
    }
  }, [])

  function beginComposerOp(convId: string, kind: ComposerOp): number {
    const token = ++opToken.current
    let byKind = inflightOps.current.get(convId)
    if (!byKind) {
      byKind = new Map()
      inflightOps.current.set(convId, byKind)
    }
    byKind.set(kind, token)
    return token
  }

  function endComposerOp(convId: string, kind: ComposerOp, token: number) {
    const byKind = inflightOps.current.get(convId)
    if (byKind?.get(kind) === token) {
      byKind.delete(kind)
      if (byKind.size === 0) inflightOps.current.delete(convId)
    }
  }

  function convHasOp(convId: string | null | undefined, kind?: ComposerOp): boolean {
    if (!convId) return false
    const byKind = inflightOps.current.get(convId)
    if (!byKind) return false
    return kind ? byKind.has(kind) : byKind.size > 0
  }

  function syncComposerLocks(convId: string | null | undefined) {
    if (!mountedRef.current) return
    setBusy(convHasOp(convId, 'send') || convHasOp(convId, 'saveKey'))
    setAskBusy(convHasOp(convId, 'ask'))
    setRecovering(convHasOp(convId, 'recover'))
  }

  useEffect(() => {
    const el = scroller.current
    if (el) el.scrollTop = el.scrollHeight
  }, [conv?.messages.length, conv?.id, conv?.live_assistant, conv?.pending_ask, live?.live_assistant, live?.pending_ask])

  useEffect(() => {
    setPaths([])
    setText('')
    setPicker(null)
    setApiKey('')
    setAskText('')
    // Restore this conversation's own locks; do not inherit or clear another
    // conversation's in-flight send/save/ask/recover state.
    syncComposerLocks(conv?.id)
  }, [conv?.id])

  if (!conv) return null

  const phase = (conv.run_phase || undefined) as string | undefined
  const agentMode = conv.agent_loop || conv.settings?.agent_mode
  const block = composeState(phase, conv.key_required, agentMode, conv.pending_rollback)
  const canType = block.ok && !busy
  const sendEnabled = canType && (block.kind !== 'paused' || text.trim().length > 0)
  const pendingAsk = (live?.session_id && (live.session_id === conv.current_session_id || live.session_id === conv.run_session_id)
    ? live.pending_ask
    : '') || conv.pending_ask || ''
  const liveAssistant = (live?.session_id && (live.session_id === conv.current_session_id || live.session_id === conv.run_session_id)
    ? live.live_assistant
    : '') || conv.live_assistant || ''
  const visibleMessages = pendingAsk
    ? (conv.messages ?? []).filter((m, i, arr) => !(i === arr.length - 1 && m.kind === 'ask' && m.text === pendingAsk))
    : (conv.messages ?? [])

  async function submitAsk() {
    if (!askText.trim()) return
    const sourceConvId = conv!.id
    const token = beginComposerOp(sourceConvId, 'ask')
    syncComposerLocks(useStore.getState().currentConvId)
    let ok = false
    try {
      if (askText.trim() === '暂停' || askText.trim().toLowerCase() === '/pause') {
        ok = await pause()
      } else {
        ok = await answer(askText)
      }
    } finally {
      endComposerOp(sourceConvId, 'ask', token)
      syncComposerLocks(useStore.getState().currentConvId)
    }
    if (ok && useStore.getState().currentConvId === sourceConvId && !convHasOp(sourceConvId, 'ask')) {
      setAskText('')
    }
  }

  async function lookup(query: string, start: number) {
    const gen = ++searchGen.current
    try {
      const got = await api.searchConversationFiles(conv!.id, query, 20)
      if (searchGen.current !== gen) return
      setPicker({ start, query, items: got.entries ?? [] })
      setHighlight(0)
    } catch {
      if (searchGen.current !== gen) return
      setPicker({ start, query, items: [] })
    }
  }

  function onTextChange(value: string, cursor: number) {
    setText(value)
    const m = mentionQuery(value, cursor)
    if (!m) {
      setPicker(null)
      return
    }
    void lookup(m.query, m.start)
  }

  function addPath(entry: FileRef, mentionStart?: number) {
    setPaths((cur) => (cur.some((p) => p.path === entry.path) ? cur : [...cur, entry]))
    if (mentionStart != null && area.current) {
      const cursor = area.current.selectionStart
      const next = text.slice(0, mentionStart) + text.slice(cursor)
      setText(next)
      requestAnimationFrame(() => {
        const pos = mentionStart
        area.current?.focus()
        area.current?.setSelectionRange(pos, pos)
      })
    }
    setPicker(null)
  }

  async function send(mode?: MessageMode, e?: FormEvent) {
    e?.preventDefault()
    if (busy || !block.ok) return
    const resolved: MessageMode =
      mode ?? (block.kind === 'steer' || block.kind === 'paused' ? 'steer' : 'start')
    if (block.kind === 'paused' && resolved !== 'steer' && resolved !== 'takeover') return
    if (!text.trim()) return
    const sourceConvId = conv!.id
    const sentSelections = pendingSelections
    const token = beginComposerOp(sourceConvId, 'send')
    syncComposerLocks(useStore.getState().currentConvId)
    let ok = false
    try {
      ok = await postMessage(text, {
        paths: paths.map((p) => p.path),
        selections: sentSelections,
        mode: resolved,
      })
    } finally {
      endComposerOp(sourceConvId, 'send', token)
      syncComposerLocks(useStore.getState().currentConvId)
    }
    // Bind the success continuation to the conversation that was sent.
    // After a switch, do not clear the new conversation's composer or citations;
    // the store already dropped only the sent drafts for the source conversation.
    if (ok && useStore.getState().currentConvId === sourceConvId && !convHasOp(sourceConvId, 'send')) {
      setText('')
      setPaths([])
      setPicker(null)
    }
  }

  async function saveKey() {
    if (!apiKey.trim() || busy) return
    const sourceConvId = conv!.id
    const fixture = apiKey.trim()
    const token = beginComposerOp(sourceConvId, 'saveKey')
    syncComposerLocks(useStore.getState().currentConvId)
    let ok = false
    try {
      ok = await setConversationKey(fixture)
    } finally {
      endComposerOp(sourceConvId, 'saveKey', token)
      syncComposerLocks(useStore.getState().currentConvId)
    }
    // Only clear this conversation's key field when the save still belongs
    // to it. Never attribute the completion to a conversation the user has
    // since opened, and never use a global setBusy(false).
    if (ok && useStore.getState().currentConvId === sourceConvId && !convHasOp(sourceConvId, 'saveKey')) {
      setApiKey('')
    }
  }

  function onKeyDown(e: KeyboardEvent<HTMLTextAreaElement>) {
    if (picker && picker.items.length) {
      if (e.key === 'ArrowDown') {
        e.preventDefault()
        setHighlight((i) => (i + 1) % picker.items.length)
        return
      }
      if (e.key === 'ArrowUp') {
        e.preventDefault()
        setHighlight((i) => (i - 1 + picker.items.length) % picker.items.length)
        return
      }
      if (e.key === 'Enter' && !e.metaKey && !e.ctrlKey) {
        e.preventDefault()
        addPath(picker.items[highlight], picker.start)
        return
      }
      if (e.key === 'Escape') {
        e.preventDefault()
        setPicker(null)
        return
      }
    }
    if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) void send()
  }

  const placeholder =
    block.kind === 'blocked'
      ? block.reason
      : block.kind === 'steer'
        ? '当前正在执行。默认插入会立刻打断当前叶子（审批/问答中会先记下）；也可排队下一条，或中止并接管。输入 @ 可强制附加文件。'
        : block.kind === 'paused'
          ? '已暂停。可先续跑；也可输入调整后点「插入到当前轮」（续跑后注入），或「中止并接管」。输入 @ 可强制附加文件。'
          : '描述要做的修改。每轮会自动带上项目结构和相关文件；用 @ 可强制附加指定文件或目录'

  return (
    <div className="flex min-h-0 shrink-0 flex-col border-b border-line">
      <div className="shrink-0 border-b border-line bg-ink-850/70 px-4 py-2">
        <div className="flex items-center gap-2 text-[12px] text-slate-400">
          <span className="truncate font-mono text-[11.5px] text-slate-300" title={conv.workdir}>
            {shortWorkdir(conv.workdir)}
          </span>
          {conv.settings.model && (
            <span className="truncate font-mono text-[11px] text-slate-500">{conv.settings.model}</span>
          )}
          <span className="ml-auto text-[11px] text-slate-600">
            工作日志 {conv.work_log?.length ?? 0}/8 轮
          </span>
        </div>
        <p className="mt-1 text-[11px] leading-snug text-slate-600">{conv.context_note}</p>
      </div>

        {conv.pending_rollback && (
          <div className="mx-4 mt-3 rounded border border-amber-800/50 bg-amber-950/20 px-3 py-2 text-[12px] text-amber-100">
            <p className="font-medium">撤销未完成，工作区可能处于部分恢复状态</p>
            <p className="mt-1 text-amber-100/90">
              {conv.pending_rollback.message ||
                '已保留操作日志和备份。在恢复完成前会阻止新的写入，以免覆盖未完成的恢复。不会执行 git reset。'}
            </p>
            {conv.pending_rollback.conflict_paths && conv.pending_rollback.conflict_paths.length > 0 && (
              <p className="mt-1 text-[11px] text-amber-200/80">
                检测到外部修改，未覆盖：{conv.pending_rollback.conflict_paths.join('、')}
              </p>
            )}
            <button
              type="button"
              className="btn mt-2 px-2 py-1 text-[11px]"
              disabled={recovering}
              onClick={async () => {
                const sourceConvId = conv.id
                const token = beginComposerOp(sourceConvId, 'recover')
                syncComposerLocks(useStore.getState().currentConvId)
                try {
                  await recoverRollback()
                } finally {
                  endComposerOp(sourceConvId, 'recover', token)
                  syncComposerLocks(useStore.getState().currentConvId)
                }
              }}
            >
              {recovering ? '正在恢复…' : '尝试恢复工作区'}
            </button>
          </div>
        )}

      <div ref={scroller} className="max-h-[34vh] min-h-[96px] space-y-2 overflow-y-auto px-4 py-3">
        {(!conv.messages || conv.messages.length === 0) && (
          <p className="py-4 text-center text-[12.5px] text-slate-600">
            在下方输入需求，divvy 会在此项目里规划并执行修改。不必每次 @，需要精确指定时再用 @。结束后可继续提修改，不必重填模型信息。
          </p>
        )}
        {visibleMessages.map((m) => (
          <MessageBubble key={m.id} msg={m} convId={conv.id} />
        ))}
        {liveAssistant && (
          <div className="rounded-md border border-brand-800/40 bg-brand-950/20 px-3 py-2">
            <div className="flex items-baseline gap-2">
              <span className="text-[10.5px] font-medium uppercase tracking-wider text-slate-500">模型输出</span>
              <span className="ml-auto h-1.5 w-1.5 animate-pulse rounded-full bg-brand-400" />
            </div>
            <p className="mt-1 whitespace-pre-wrap break-words text-[12.5px] leading-relaxed text-slate-100">
              {liveAssistant}
            </p>
          </div>
        )}
        {pendingAsk && (
          <div className="rounded-md border border-orange-800/50 bg-orange-950/20 px-3 py-2">
            <div className="flex items-center gap-2">
              <span className="h-2 w-2 animate-pulse rounded-full bg-orange-400" />
              <span className="text-[10.5px] font-medium uppercase tracking-wider text-orange-200/90">模型在向你提问</span>
            </div>
            <p className="mt-1.5 whitespace-pre-wrap break-words text-[12.5px] leading-relaxed text-slate-100">{pendingAsk}</p>
            <textarea
              className="input mt-2 min-h-[72px] resize-y text-[12.5px]"
              placeholder="输入回答后发送；输入「暂停」可保存并停止（不会把暂停当成对模型的回答）"
              value={askText}
              onChange={(e) => setAskText(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) void submitAsk()
              }}
            />
            {actionError && <p className="mt-1.5 text-[12px] text-rose-300">{actionError}</p>}
            <div className="mt-2 flex justify-end gap-2">
              <button type="button" className="btn-ghost px-2 py-1 text-[11px]" disabled={askBusy} onClick={() => void pause()}>
                暂停运行
              </button>
              <button
                type="button"
                className="btn-primary px-3 py-1 text-[11px]"
                disabled={askBusy || !askText.trim()}
                onClick={() => void submitAsk()}
              >
                发送回答
              </button>
            </div>
          </div>
        )}
      </div>

      {conv.key_required && (
        <div className="border-t border-amber-900/40 bg-amber-950/20 px-4 py-2">
          <p className="mb-1.5 text-[11.5px] text-amber-200/90">
            服务重启后 API key 不在内存中。重新填写后即可继续（不会写入历史）。
          </p>
          <div className="flex gap-2">
            <input
              type="password"
              className="input flex-1 py-1.5 font-mono text-[12px]"
              placeholder="sk-..."
              value={apiKey}
              autoComplete="off"
              onChange={(e) => setApiKey(e.target.value)}
            />
            <button className="btn-primary px-3 py-1.5 text-[12px]" disabled={busy || !apiKey.trim()} onClick={() => void saveKey()}>
              保存到内存
            </button>
          </div>
        </div>
      )}

      {conv.steer && (
        <div className="flex items-start gap-2 border-t border-violet-900/40 bg-violet-950/20 px-4 py-2">
          <div className="min-w-0 flex-1">
            <p className="text-[11.5px] text-violet-200/90">{steerPendingText(agentMode, conv.in_tool)}</p>
            <p className="truncate text-[12px] text-slate-300">{conv.steer.text}</p>
          </div>
          <button type="button" className="btn px-2 py-1 text-[11px]" disabled={busy} onClick={() => void cancelSteer()}>
            取消调整
          </button>
        </div>
      )}

      {conv.queue && (
        <div className="flex items-start gap-2 border-t border-sky-900/40 bg-sky-950/20 px-4 py-2">
          <div className="min-w-0 flex-1">
            <p className="text-[11.5px] text-sky-200/90">已排队到下一轮，当前任务完成、失败或中止后会自动开始（暂停不会自动开跑；与「插入当前轮」不是同一件事）</p>
            <p className="truncate text-[12px] text-slate-300">{conv.queue.text}</p>
          </div>
          <button type="button" className="btn px-2 py-1 text-[11px]" disabled={busy} onClick={() => void cancelQueue()}>
            取消排队
          </button>
        </div>
      )}

      <form onSubmit={(e) => void send(undefined, e)} className="relative border-t border-line px-4 py-2">
        <SelectionChips items={pendingSelections} onRemove={removeSelection} />
        <AttachmentChips refs={paths} onRemove={(index) => setPaths((cur) => cur.filter((_, i) => i !== index))} />
        <textarea
          ref={area}
          className="input mt-1.5 min-h-[64px] resize-y text-[12.5px]"
          placeholder={placeholder}
          value={text}
          disabled={busy || block.kind === 'blocked'}
          onChange={(e) => onTextChange(e.target.value, e.target.selectionStart)}
          onKeyUp={(e) => {
            const t = e.currentTarget
            if (mentionQuery(t.value, t.selectionStart) && !picker) onTextChange(t.value, t.selectionStart)
          }}
          onKeyDown={onKeyDown}
        />
        {picker && (
          <div className="absolute bottom-[calc(100%-8px)] left-4 right-4 z-20 max-h-48 overflow-y-auto rounded-md border border-line bg-ink-850 shadow-lg">
            {picker.items.length === 0 ? (
              <p className="px-3 py-2 text-[11.5px] text-slate-500">没有匹配的项目文件（不会越出本项目目录）</p>
            ) : (
              picker.items.map((item, i) => (
                <button
                  key={item.path}
                  type="button"
                  className={`flex w-full items-center gap-2 px-3 py-1.5 text-left font-mono text-[11.5px] ${
                    i === highlight ? 'bg-brand-950/60 text-slate-100' : 'text-slate-300 hover:bg-ink-800'
                  }`}
                  onMouseEnter={() => setHighlight(i)}
                  onClick={() => addPath(item, picker.start)}
                >
                  <span className="text-slate-500">{item.type === 'dir' ? '目录' : '文件'}</span>
                  <span className="truncate">{item.path}</span>
                </button>
              ))
            )}
            <p className="border-t border-line px-3 py-1 text-[10.5px] text-slate-600">
              引用内容有上限：单文件 64KiB，合计 256KiB；二进制或过大文件会被跳过，路径不能逃出项目目录。
            </p>
          </div>
        )}
        <div className="mt-2 flex flex-wrap items-center gap-2">
          <button
            type="button"
            className="btn px-2 py-1.5 text-[12px]"
            disabled={busy || block.kind === 'blocked'}
            onClick={() => {
              const next = text + (text && !/\s$/.test(text) ? ' @' : '@')
              setText(next)
              void lookup('', next.lastIndexOf('@'))
              requestAnimationFrame(() => {
                area.current?.focus()
                const pos = next.length
                area.current?.setSelectionRange(pos, pos)
              })
            }}
          >
            @ 引用文件
          </button>
          {block.ok && block.kind === 'paused' && (
            <button
              type="button"
              className="btn-primary px-3 py-1.5 text-[12px]"
              disabled={busy}
              onClick={() => void resumeConversation()}
            >
              续跑当前任务
            </button>
          )}
          {block.ok && block.kind === 'paused' && (
            <button
              type="button"
              className="btn-primary px-3 py-1.5 text-[12px]"
              disabled={busy || !text.trim()}
              title="记下这条调整，续跑后再注入当前轮，不中止已完成的工作，也不会绕过审批。"
              onClick={() => void send('steer')}
            >
              插入到当前轮
            </button>
          )}
          {block.ok && block.canQueue && (
            <button
              type="button"
              className="btn px-3 py-1.5 text-[12px]"
              disabled={busy || !text.trim()}
              title="不打断当前轮。等这轮完成、失败或中止后再自动开始这一条；与插入当前轮不同。"
              onClick={() => void send('queue')}
            >
              排队下一条
            </button>
          )}
          {block.ok && block.canTakeover && (
            <button
              type="button"
              className="btn px-3 py-1.5 text-[12px]"
              disabled={busy || !text.trim()}
              title="中止当前任务，并用这条新指令立即开始；已有的排队和未注入的调整会被取消。计划/叶子审批在新一轮里照常生效。"
              onClick={() => void send('takeover')}
            >
              中止并接管
            </button>
          )}
          {block.ok && block.kind !== 'paused' && (
            <button
              type="submit"
              className="btn-primary ml-auto px-3 py-1.5 text-[12px]"
              disabled={!sendEnabled || !text.trim()}
              title={block.kind === 'steer' ? (agentMode === 'direct' ? '将在当前工具结束后的下一模型步骤注入同一循环；不会取消正在执行的工具，已完成修改保留。' : '执行中会立刻打断当前叶子并注入；审批或问答中会先记下，结束后再生效。已完成工作保留。') : undefined}
            >
              {busy && <SpinnerIcon width={12} height={12} />}
              {block.primary}
            </button>
          )}
          {block.ok && <span className="text-[11px] text-slate-600">⌘/Ctrl + Enter</span>}
        </div>
        {block.ok && block.hint && <p className="mt-1.5 text-[11.5px] leading-snug text-slate-500">{block.hint}</p>}
        {!block.ok && !conv.key_required && <p className="mt-1.5 text-[11.5px] text-amber-300/80">{block.reason}</p>}
      </form>
    </div>
  )
}
