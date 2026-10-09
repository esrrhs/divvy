import { useState } from 'react'
import { useStore } from '../store'
import type { StreamEvent } from '../types'
import { compactTokens, treeStats } from '../lib/tree'
import { NewSessionForm } from './NewSessionForm'
import { PhaseBadge } from './StatusBadge'
import { TaskTreeView } from './TaskTreeView'
import { EventTimeline } from './EventTimeline'
import { PlanReviewBar } from './PlanReviewBar'
import { LeafApprovalCard } from './LeafApprovalCard'
import { RunControls } from './RunControls'
import { FileViewer } from './FileViewer'
import { BranchIcon, TerminalIcon } from './icons'

function TreeHeader() {
  const tree = useStore((s) => s.tree)!
  const s = treeStats(tree)
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
      {isLiveSession && phase !== 'done' && phase !== 'failed' && phase !== 'paused' && (
        <RunControls />
      )}

      <div className="flex min-h-0 flex-1 flex-col">
        <div className="flex min-h-0 flex-1 flex-col">
          {tree ? (
            <>
              <TreeHeader />
              <div className="min-h-0 flex-1 overflow-y-auto">
                <TaskTreeView tree={tree} />
              </div>
            </>
          ) : (
            <div className="flex flex-1 items-center justify-center text-[12.5px] text-slate-600">
              等待第一个任务树快照…
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

export function CenterMain() {
  const currentId = useStore((s) => s.currentId)
  const fileView = useStore((s) => s.fileView)

  if (!currentId) {
    return (
      <div className="flex flex-1 items-start justify-center overflow-y-auto px-6 py-10">
        <div className="w-full max-w-[640px]">
          <div className="mb-6 text-center">
            <h1 className="text-[20px] font-semibold text-slate-100">divvy</h1>
            <p className="mt-1.5 text-[13px] text-slate-500">
              把开发目标交给弱模型拆解执行——实时监督、审批 diff、随时干预
            </p>
          </div>
          <div className="rounded-xl border border-line bg-ink-850/80 p-5 shadow-2xl shadow-black/30">
            <NewSessionForm />
          </div>
        </div>
      </div>
    )
  }

  // A workspace file opened from the left file browser replaces the session
  // view in the center pane; closing returns to the live/tree view.
  if (fileView) {
    return <FileViewer sessionId={fileView.sessionId} path={fileView.path} />
  }

  return <SessionView />
}
