import { useMemo, useRef, useState } from 'react'
import { useStore } from '../store'
import type { StreamEvent } from '../types'
import { clampRowSplitRatio, clampSplitRatio } from '../lib/conversation'
import { compactTokens, treeStats } from '../lib/tree'
import { ProjectForm } from './ProjectForm'
import { ConversationView } from './ConversationView'
import { PhaseBadge } from './StatusBadge'
import { TaskTreeView } from './TaskTreeView'
import { EventTimeline } from './EventTimeline'
import { PlanReviewBar } from './PlanReviewBar'
import { LeafApprovalCard } from './LeafApprovalCard'
import { RunControls } from './RunControls'
import { FileViewer } from './FileViewer'
import { BranchIcon, TerminalIcon } from './icons'

function PausedBar() {
  const resumeConversation = useStore((s) => s.resumeConversation)
  const resumeSession = useStore((s) => s.resumeSession)
  const currentConvId = useStore((s) => s.currentConvId)
  const currentId = useStore((s) => s.currentId)
  const [busy, setBusy] = useState(false)
  return (
    <div className="flex shrink-0 items-center gap-3 border-b border-amber-800/50 bg-amber-950/20 px-5 py-2">
      <p className="text-[12.5px] text-amber-200">任务已暂停。续跑会继续上次未完成的执行；发送新需求前请先续跑或中止。</p>
      <button
        className="btn-primary ml-auto px-3 py-1 text-[12px]"
        disabled={busy}
        onClick={async () => {
          setBusy(true)
          if (currentConvId) await resumeConversation()
          else if (currentId) await resumeSession(currentId)
          setBusy(false)
        }}
      >
        续跑
      </button>
    </div>
  )
}

function TreeHeader() {
  const tree = useStore((s) => s.tree)!
  // SessionView also subscribes to the events stream, so this header rerenders
  // on every unrelated event. Memoizing keeps the O(n) stats scan tied to the
  // tree reference alone (snapshots replace it; plain events do not).
  const s = useMemo(() => treeStats(tree), [tree])
  return (
    <div className="flex shrink-0 items-center gap-3 border-b border-line bg-ink-850/60 px-4 py-2 text-[11.5px] text-slate-500">
      <BranchIcon width={13} height={13} className="text-slate-500" />
      <span>
        叶子 <span className="font-mono text-slate-300">{s.leavesDone}/{s.leaves}</span>
      </span>
      <span>
        调用 <span className="font-mono text-slate-300">{s.calls}</span>
      </span>
      <span>
        Token <span className="font-mono text-slate-300">{compactTokens(s.tokens)}</span>
      </span>
      {s.failed > 0 && <span className="text-rose-400">{s.failed} 失败</span>}
      {s.running > 0 && <span className="text-brand-400">{s.running} 进行中</span>}
      <span className="ml-auto truncate font-mono text-[10.5px] text-slate-600">{tree.model_name}</span>
    </div>
  )
}

function SessionView() {
  const live = useStore((s) => s.live)
  const tree = useStore((s) => s.tree)
  const events = useStore((s) => s.events)
  const currentId = useStore((s) => s.currentId)
  const [showEvents, setShowEvents] = useState(true)
  const phase = live?.phase

  const isLiveSession = !!live

  return (
    <div className="flex flex-1 flex-col overflow-hidden">
      <div className="flex shrink-0 items-center gap-3 border-b border-line bg-ink-850/60 px-4 py-2">
        {live ? <PhaseBadge phase={live.phase} /> : <span className="text-[12px] text-slate-500">历史会话（只读）</span>}
        {live?.leaf_approving && (
          <span className="rounded border border-fuchsia-700/50 bg-fuchsia-950/30 px-1.5 py-0.5 text-[11px] text-fuchsia-300">
            审批 {live.leaf_approving}
          </span>
        )}
        {live?.error && (
          <span className="truncate text-[11.5px] text-rose-400/90" title={live.error}>
            {live.error}
          </span>
        )}
        <button
          onClick={() => setShowEvents((v) => !v)}
          className="ml-auto flex items-center gap-1.5 rounded px-2 py-0.5 text-[11.5px] text-slate-500 hover:bg-ink-800 hover:text-slate-300"
        >
          <TerminalIcon width={12} height={12} />
          {showEvents ? '收起事件流' : '展开事件流'}
          <span className="rounded bg-ink-700 px-1 font-mono text-[10px]">{events.length}</span>
        </button>
      </div>

      {isLiveSession && phase === 'plan_review' && <PlanReviewBar />}
      {isLiveSession && phase === 'leaf_approval' && <LeafApprovalCard />}
      {isLiveSession && phase === 'paused' && <PausedBar />}
      {isLiveSession && phase !== 'done' && phase !== 'failed' && phase !== 'paused' && (
        <RunControls />
      )}

      <div className="flex min-h-0 flex-1 flex-col">
        <div className="flex min-h-0 flex-1 flex-col">
          {tree ? (
            <>
              <TreeHeader />
              {/* TaskTreeView owns scrolling (windowed rendering needs the
                  scroll source inside the component). */}
              <div className="min-h-0 flex-1">
                <TaskTreeView tree={tree} />
              </div>
            </>
          ) : currentId ? (
            <div className="flex flex-1 items-center justify-center text-[12.5px] text-slate-600">
              等待第一个任务树快照…
            </div>
          ) : (
            <div className="flex flex-1 items-center justify-center text-[12.5px] text-slate-600">
              发送第一条需求后，这里会显示任务树与执行状态。
            </div>
          )}
        </div>

        {showEvents && (
          <div className="flex min-h-0 flex-[0.42] flex-col border-t border-line bg-ink-900">
            <div className="flex shrink-0 items-center px-5 pt-2 text-[11px] uppercase tracking-wider text-slate-600">
              事件流
            </div>
            <EventTimeline events={events as StreamEvent[]} />
          </div>
        )}
      </div>
    </div>
  )
}

// Persisted conversation/viewer width ratio for the split layout.
const SPLIT_KEY = 'divvy.splitRatio'

// Persisted conversation/session height ratio (chat above, tree + event
// timeline below).
const ROW_KEY = 'divvy.rowSplitRatio'

function initialSplitRatio(): number {
  try {
    const raw = Number(localStorage.getItem(SPLIT_KEY) ?? '')
    return clampSplitRatio(raw || Number.NaN)
  } catch {
    return 0.4
  }
}

function initialRowSplitRatio(): number {
  try {
    const raw = Number(localStorage.getItem(ROW_KEY) ?? '')
    return clampRowSplitRatio(raw || Number.NaN)
  } catch {
    return 0.6
  }
}

export function CenterMain() {
  const currentId = useStore((s) => s.currentId)
  const currentConvId = useStore((s) => s.currentConvId)
  const fileView = useStore((s) => s.fileView)
  const [splitRatio, setSplitRatio] = useState(initialSplitRatio)
  const [rowRatio, setRowRatio] = useState(initialRowSplitRatio)
  const splitRowRef = useRef<HTMLDivElement | null>(null)
  const dragStart = useRef<{ x: number; ratio: number } | null>(null)
  const rowColRef = useRef<HTMLDivElement | null>(null)
  const rowDragStart = useRef<{ y: number; ratio: number } | null>(null)

  // The divider is the width allocation knob: drag it, or nudge with arrow
  // keys. The ratio lives in localStorage so a reload keeps the layout.
  function startDrag(e: React.MouseEvent) {
    e.preventDefault()
    const row = splitRowRef.current
    if (!row) return
    dragStart.current = { x: e.clientX, ratio: splitRatio }
    const onMove = (ev: MouseEvent) => {
      const w = row.getBoundingClientRect().width
      const start = dragStart.current
      if (!start || w <= 0) return
      setSplitRatio(clampSplitRatio(start.ratio + (ev.clientX - start.x) / w))
    }
    const onUp = () => {
      window.removeEventListener('mousemove', onMove)
      window.removeEventListener('mouseup', onUp)
      setSplitRatio((r) => {
        try {
          localStorage.setItem(SPLIT_KEY, String(r))
        } catch {
          // Private-mode storage: the layout just won't persist.
        }
        return r
      })
    }
    window.addEventListener('mousemove', onMove)
    window.addEventListener('mouseup', onUp)
  }

  function nudge(delta: number) {
    setSplitRatio((r) => {
      const next = clampSplitRatio(r + delta)
      try {
        localStorage.setItem(SPLIT_KEY, String(next))
      } catch {
        // Same as above: persistence is best effort.
      }
      return next
    })
  }

  // The row divider is the height allocation knob between the conversation
  // and the session pane (tree + event timeline). The conversation's natural
  // height used to eat the whole column whenever messages accumulated,
  // squeezing the event timeline to a ~30px strip where entries were
  // unreadable.
  function startRowDrag(e: React.MouseEvent) {
    e.preventDefault()
    const col = rowColRef.current
    if (!col) return
    rowDragStart.current = { y: e.clientY, ratio: rowRatio }
    const onMove = (ev: MouseEvent) => {
      const h = col.getBoundingClientRect().height
      const start = rowDragStart.current
      if (!start || h <= 0) return
      setRowRatio(clampRowSplitRatio(start.ratio + (ev.clientY - start.y) / h))
    }
    const onUp = () => {
      window.removeEventListener('mousemove', onMove)
      window.removeEventListener('mouseup', onUp)
      setRowRatio((r) => {
        try {
          localStorage.setItem(ROW_KEY, String(r))
        } catch {
          // Private-mode storage: the layout just won't persist.
        }
        return r
      })
    }
    window.addEventListener('mousemove', onMove)
    window.addEventListener('mouseup', onUp)
  }

  function nudgeRow(delta: number) {
    setRowRatio((r) => {
      const next = clampRowSplitRatio(r + delta)
      try {
        localStorage.setItem(ROW_KEY, String(next))
      } catch {
        // Same as above: persistence is best effort.
      }
      return next
    })
  }

  if (!currentConvId && !currentId) {
    return (
      <div className="flex flex-1 items-start justify-center overflow-y-auto px-6 py-10">
        <div className="w-full max-w-[640px]">
          <div className="mb-6 text-center">
            <h1 className="text-[20px] font-semibold text-slate-100">divvy</h1>
            <p className="mt-1.5 text-[13px] text-slate-500">
              选择已有项目，把需求交给弱模型拆解执行——同一对话里连续修改、监督审批
            </p>
          </div>
          <div className="rounded-xl border border-line bg-ink-850/80 p-5 shadow-2xl shadow-black/30">
            <ProjectForm />
          </div>
        </div>
      </div>
    )
  }

  // Conversation above, tree + event timeline below, with a draggable height
  // divider. Shared by the plain center pane and the split layout's left
  // column so the timeline keeps a usable height in both.
  function chatAndSessionStack() {
    return (
      <div ref={rowColRef} className="flex min-h-0 flex-1 flex-col">
        {currentConvId && (
          <>
            <div
              className="flex min-h-0 flex-col overflow-hidden"
              style={{ flex: `0 0 ${rowRatio * 100}%` }}
            >
              <ConversationView />
            </div>
            <div
              role="separator"
              aria-orientation="horizontal"
              aria-label="调整对话与执行面板高度"
              tabIndex={0}
              title="拖动调整高度；↑/↓ 微调"
              // z-10 keeps the 4px strip grabbable even when the conversation's
              // content minimum (header + composer) briefly paints over it on
              // short viewports.
              className="relative z-10 h-1 shrink-0 cursor-row-resize bg-ink-800 hover:bg-brand-700/70 focus:bg-brand-700/70 focus:outline-none"
              onMouseDown={startRowDrag}
              onKeyDown={(e) => {
                if (e.key === 'ArrowUp') {
                  e.preventDefault()
                  nudgeRow(-0.02)
                }
                if (e.key === 'ArrowDown') {
                  e.preventDefault()
                  nudgeRow(0.02)
                }
              }}
            />
          </>
        )}
        <div className="flex min-h-0 flex-1 flex-col">
          {currentId || currentConvId ? <SessionView /> : null}
        </div>
      </div>
    )
  }

  // Chat and editor coexist — the Cursor layout. Opening a file used to
  // replace the conversation entirely, which threw the composer (and with it
  // the auto-context chip and the whole loop) off screen: to send a message
  // you had to close the file you were looking at. Now the conversation keeps
  // its column while the file sits beside it; 关闭 hands the space back. A
  // workspace file with no active conversation keeps the full-pane behavior.
  if (fileView && currentConvId) {
    return (
      <div ref={splitRowRef} className="flex min-h-0 flex-1">
        <div
          className="flex min-h-0 min-w-0 flex-col border-r border-line"
          style={{ width: `${splitRatio * 100}%` }}
        >
          {chatAndSessionStack()}
        </div>
        <div
          role="separator"
          aria-orientation="vertical"
          aria-label="调整对话与文件面板宽度"
          tabIndex={0}
          title="拖动调整宽度；←/→ 微调"
          className="w-1 shrink-0 cursor-col-resize bg-ink-800 hover:bg-brand-700/70 focus:bg-brand-700/70 focus:outline-none"
          onMouseDown={startDrag}
          onKeyDown={(e) => {
            if (e.key === 'ArrowLeft') {
              e.preventDefault()
              nudge(-0.02)
            }
            if (e.key === 'ArrowRight') {
              e.preventDefault()
              nudge(0.02)
            }
          }}
        />
        <div className="flex min-h-0 min-w-0 flex-1 flex-col bg-ink-900">
          <FileViewer sessionId={fileView.sessionId} path={fileView.path} />
        </div>
      </div>
    )
  }

  // A workspace file opened from the left file browser replaces the session
  // view in the center pane; closing returns to the live/tree view.
  if (fileView) {
    return <FileViewer sessionId={fileView.sessionId} path={fileView.path} />
  }

  return chatAndSessionStack()
}
