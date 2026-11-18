import { useMemo } from 'react'
import type { SseStatus } from '../api/sse'
import { compactTokens, treeStats } from '../lib/tree'
import { useStore } from '../store'
import type { PhaseSnapshot } from '../types'
import { PhaseBadge } from './StatusBadge'

const connMeta: Record<SseStatus, { dot: string; label: string }> = {
  open: { dot: 'bg-emerald-400', label: '已连接' },
  connecting: { dot: 'bg-amber-400 animate-pulse', label: '连接中' },
  closed: { dot: 'bg-slate-600', label: '未连接' },
}

export function TopBar({
  live,
  sseStatus,
}: {
  live: PhaseSnapshot | null
  sseStatus: SseStatus
}) {
  const conn = connMeta[sseStatus]
  const conversation = useStore((s) => s.conversation)
  const tree = useStore((s) => s.tree)
  const stats = useMemo(() => (tree ? treeStats(tree) : null), [tree])
  const model = conversation?.settings.model

  return (
    <header className="flex h-12 shrink-0 items-center justify-between border-b border-line bg-ink-850 px-4">
      <div className="flex min-w-0 items-center gap-3">
        <span className="shrink-0 font-mono text-[13px] font-semibold tracking-wide text-slate-200">
          divvy
        </span>
        {live && (
          <>
            <span className="h-3.5 w-px bg-line" />
            <PhaseBadge phase={live.phase} />
            <span className="hidden truncate font-mono text-[11.5px] text-slate-500 lg:inline">
              {live.session_id}
            </span>
          </>
        )}
      </div>

      <div className="flex items-center gap-4 text-[11.5px] text-slate-500">
        {/* Live facts only: the conversation's model and the tree's token
            totals. There is no cost source in the tree snapshot, so no $ —
            an empty placeholder badge is worse than none. */}
        {model && (
          <span className="hidden max-w-[180px] truncate font-mono md:inline" title={model}>
            {model}
          </span>
        )}
        {stats && stats.calls > 0 && (
          <span className="hidden font-mono md:inline">
            {stats.calls} 次调用 · {compactTokens(stats.tokens)} tok
          </span>
        )}
        <span className="flex items-center gap-1.5">
          <span className={`h-1.5 w-1.5 rounded-full ${conn.dot}`} />
          {conn.label}
        </span>
      </div>
    </header>
  )
}
