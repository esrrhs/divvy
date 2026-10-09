import type { NodeState, TaskNode, TaskTree } from '../types'

// Ordered root-down traversal, following children_ids (the tree stores a
// map; ordering explicitly keeps rendering stable as snapshots replace each
// other).
export function traverse(tree: TaskTree): TaskNode[] {
  const out: TaskNode[] = []
  const seen = new Set<string>()
  const walk = (id: string) => {
    if (seen.has(id)) return
    seen.add(id)
    const n = tree.nodes[id]
    if (!n) return
    out.push(n)
    for (const cid of n.children_ids ?? []) walk(cid)
  }
  walk(tree.root_id)
  return out
}

export interface TreeStats {
  total: number
  leaves: number
  leavesDone: number
  tokens: number
  calls: number
  failed: number
  running: number
  pending: number
}

export function treeStats(tree: TaskTree): TreeStats {
  const s: TreeStats = {
    total: 0,
    leaves: 0,
    leavesDone: 0,
    tokens: 0,
    calls: 0,
    failed: 0,
    running: 0,
    pending: 0,
  }
  for (const n of Object.values(tree.nodes)) {
    s.total++
    s.tokens += n.token_usage?.total_tokens ?? 0
    s.calls += n.token_usage?.calls ?? 0
    if (n.type === 'LEAF') {
      s.leaves++
      if (n.state === 'COMPLETED') s.leavesDone++
    }
    if (n.state === 'FAILED') s.failed++
    if (n.state === 'RUNNING' || n.state === 'VERIFYING' || n.state === 'DECOMPOSING')
      s.running++
    if (n.state === 'PENDING') s.pending++
  }
  return s
}

export function formatDuration(fromIso?: string, toIso?: string): string {
  if (!fromIso) return ''
  const from = Date.parse(fromIso)
  if (!Number.isFinite(from)) return ''
  const to = toIso ? Date.parse(toIso) : Date.now()
  const ms = Math.max(0, to - from)
  if (ms < 1000) return `${ms}ms`
  const s = ms / 1000
  if (s < 60) return `${s.toFixed(s < 10 ? 1 : 0)}s`
  const m = Math.floor(s / 60)
  const rs = Math.round(s % 60)
  return `${m}m ${rs}s`
}

export function compactTokens(n: number): string {
  if (n <= 0) return '0'
  if (n < 1000) return String(n)
  if (n < 1_000_000) return `${(n / 1000).toFixed(n < 10_000 ? 1 : 0)}k`
  return `${(n / 1_000_000).toFixed(2)}M`
}

export const stateMeta: Record<
  NodeState,
  { color: string; ring: string; label: string; spin?: boolean }
> = {
  PENDING: { color: 'text-slate-500', ring: 'border-slate-600 bg-ink-750', label: '待运行' },
  DECOMPOSING: { color: 'text-amber-300', ring: 'border-amber-500/60 bg-amber-500/10', label: '拆解中', spin: true },
  RUNNING: { color: 'text-brand-400', ring: 'border-brand-500/60 bg-brand-600/10', label: '运行中', spin: true },
  VERIFYING: { color: 'text-cyan-300', ring: 'border-cyan-500/60 bg-cyan-500/10', label: '验收中', spin: true },
  COMPLETED: { color: 'text-emerald-300', ring: 'border-emerald-600/50 bg-emerald-600/10', label: '完成' },
  FAILED: { color: 'text-rose-300', ring: 'border-rose-700/60 bg-rose-700/15', label: '失败' },
  SKIPPED: { color: 'text-slate-600', ring: 'border-slate-700 bg-ink-800', label: '跳过' },
}
