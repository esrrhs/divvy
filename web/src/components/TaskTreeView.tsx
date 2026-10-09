import { useMemo, useState } from 'react'
import type { TaskNode, TaskTree } from '../types'
import { compactTokens, formatDuration, stateMeta, traverse } from '../lib/tree'
import { useStore } from '../store'
import { BranchIcon, CheckIcon, DocIcon, SpinnerIcon, XIcon } from './icons'

function StateGlyph({ node }: { node: TaskNode }) {
  const meta = stateMeta[node.state]
  if (node.state === 'COMPLETED')
    return <CheckIcon width={11} height={11} className="shrink-0 text-emerald-400" />
  if (node.state === 'FAILED')
    return <XIcon width={11} height={11} className="shrink-0 text-rose-400" />
  if (meta.spin)
    return <SpinnerIcon width={11} height={11} className={`shrink-0 ${meta.color}`} />
  return node.type === 'LEAF' ? (
    <DocIcon width={11} height={11} className="shrink-0 text-slate-600" />
  ) : (
    <BranchIcon width={11} height={11} className="shrink-0 text-slate-600" />
  )
}

function NodeRow({ node, collapsed, onToggle }: {
  node: TaskNode
  collapsed: boolean
  onToggle: () => void
}) {
  const selectedId = useStore((s) => s.selectedNodeId)
  const selectNode = useStore((s) => s.selectNode)
  const meta = stateMeta[node.state]
  const hasChildren = (node.children_ids?.length ?? 0) > 0
  const selected = selectedId === node.id
  const usage = node.token_usage
  const retries = node.retry_count ?? 0

  return (
    <div
      role="treeitem"
      aria-selected={selected}
      aria-expanded={hasChildren ? !collapsed : undefined}
      onClick={() => selectNode(node.id)}
      className={`group cursor-pointer rounded-md border px-2 py-1.5 transition ${
        selected
          ? 'border-brand-600/70 bg-brand-600/10'
          : 'border-transparent hover:border-line hover:bg-ink-800/70'
      }`}
      style={{ marginLeft: node.depth * 14 }}
    >
      <div className="flex items-center gap-1.5">
        <button
          type="button"
          onClick={(e) => {
            e.stopPropagation()
            if (hasChildren) onToggle()
          }}
          className={`flex h-4 w-4 shrink-0 items-center justify-center rounded text-slate-500 ${
            hasChildren ? 'hover:text-slate-300' : 'invisible'
          }`}
          aria-label={collapsed ? '展开' : '折叠'}
        >
          <svg width="10" height="10" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.5">
            {hasChildren && <path d={collapsed ? 'M9 6l6 6-6 6' : 'M6 9l6 6 6-6'} />}
          </svg>
        </button>

        <StateGlyph node={node} />

        <span className={`min-w-0 flex-1 truncate text-[12.5px] ${meta.color}`} title={node.title}>
          {node.title}
        </span>

        <span className="shrink-0 font-mono text-[10px] text-slate-600">{node.id}</span>
      </div>

      <div className="ml-5 mt-0.5 flex flex-wrap items-center gap-x-2.5 gap-y-0.5 text-[10.5px] text-slate-500">
        <span className={`rounded px-1 py-px ${meta.ring} border ${meta.color}`}>{meta.label}</span>
        {node.type === 'COMPOUND' && (
          <span className="text-slate-600">
            {node.children_ids?.length ?? 0} 个子节点
          </span>
        )}
        {retries > 0 && <span className="text-amber-400/90">重试 {retries}</span>}
        {(usage?.calls ?? 0) > 0 && (
          <span className="font-mono">{compactTokens(usage.total_tokens ?? 0)} tok</span>
        )}
        {node.state !== 'PENDING' && (
          <span className="font-mono text-slate-600">
            {formatDuration(node.created_at, node.state === 'COMPLETED' || node.state === 'FAILED'
              ? node.updated_at
              : undefined)}
          </span>
        )}
        {node.integration_verified && (
          <span className="rounded border border-emerald-700/50 px-1 text-emerald-400">
            端到端验收
          </span>
        )}
      </div>

      {node.error_msg && node.state === 'FAILED' && (
        <p className="ml-5 mt-1 line-clamp-2 break-words text-[11px] leading-snug text-rose-300/80">
          {node.error_msg}
        </p>
      )}
    </div>
  )
}

export function TaskTreeView({ tree }: { tree: TaskTree }) {
  // Default-expanded compound ids. Failed/active branches stay open so the
  // user immediately sees what needs attention; finished branches can fold.
  const nodes = useMemo(() => traverse(tree), [tree])
  const [manualCollapsed, setManualCollapsed] = useState<Record<string, boolean>>({})

  const toggle = (id: string) =>
    setManualCollapsed((m) => ({ ...m, [id]: !m[id] }))

  const visible = useMemo(() => {
    const collapsedSet = new Set<string>()
    const out: TaskNode[] = []
    for (const n of nodes) {
      // Hidden if any ancestor is collapsed.
      let hidden = false
      let pid = n.parent_id
      while (pid) {
        if (collapsedSet.has(pid)) {
          hidden = true
          break
        }
        pid = tree.nodes[pid]?.parent_id
      }
      if (!hidden) out.push(n)
      const isCollapsed =
        manualCollapsed[n.id] ??
        // Auto-collapse fully completed, integration-verified compounds.
        (n.type === 'COMPOUND' &&
          n.state === 'COMPLETED' &&
          n.integration_verified)
      if (isCollapsed && (n.children_ids?.length ?? 0) > 0) collapsedSet.add(n.id)
    }
    return out
  }, [nodes, manualCollapsed, tree.nodes])

  const isCollapsed = (n: TaskNode) =>
    manualCollapsed[n.id] ??
    (n.type === 'COMPOUND' && n.state === 'COMPLETED' && n.integration_verified)

  return (
    <div role="tree" className="space-y-0.5 px-3 py-2">
      {visible.map((n) => (
        <NodeRow key={n.id} node={n} collapsed={isCollapsed(n)} onToggle={() => toggle(n.id)} />
      ))}
    </div>
  )
}
