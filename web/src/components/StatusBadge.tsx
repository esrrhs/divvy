import type { RunPhase } from '../types'

const phaseStyle: Record<RunPhase, { dot: string; text: string; label: string }> = {
  planning: { dot: 'bg-amber-400', text: 'text-amber-300', label: '规划中' },
  plan_review: { dot: 'bg-amber-400 animate-pulse', text: 'text-amber-300', label: '待审批' },
  running: { dot: 'bg-brand-400 animate-pulse', text: 'text-brand-400', label: '运行中' },
  leaf_approval: { dot: 'bg-fuchsia-400 animate-pulse', text: 'text-fuchsia-300', label: '叶子待审' },
  ask: { dot: 'bg-orange-400 animate-pulse', text: 'text-orange-300', label: '等待回答' },
  paused: { dot: 'bg-slate-500', text: 'text-slate-400', label: '已暂停' },
  done: { dot: 'bg-emerald-400', text: 'text-emerald-300', label: '已完成' },
  failed: { dot: 'bg-rose-500', text: 'text-rose-300', label: '失败/中止' },
}

export function PhaseBadge({ phase }: { phase: RunPhase }) {
  const s = phaseStyle[phase] ?? phaseStyle.running
  return (
    <span className={`inline-flex items-center gap-1.5 text-[12px] font-medium ${s.text}`}>
      <span className={`h-1.5 w-1.5 rounded-full ${s.dot}`} />
      {s.label}
    </span>
  )
}
