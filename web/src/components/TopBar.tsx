import type { SseStatus } from '../api/sse'
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
        {/* Token / cost / elapsed placeholders wired in Task 8/9. */}
        <span className="hidden font-mono md:inline">model —</span>
        <span className="hidden font-mono md:inline">tokens —</span>
        <span className="hidden font-mono md:inline">$ —</span>
        <span className="flex items-center gap-1.5">
          <span className={`h-1.5 w-1.5 rounded-full ${conn.dot}`} />
          {conn.label}
        </span>
      </div>
    </header>
  )
}
