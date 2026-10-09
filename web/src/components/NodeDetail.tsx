import type { ContractSpec, DoD, TaskNode } from '../types'
import { compactTokens, formatDuration, stateMeta } from '../lib/tree'
import { PhaseBadge } from './StatusBadge'

function Section({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <section className="border-b border-line px-4 py-3">
      <h4 className="mb-2 text-[11px] font-medium uppercase tracking-wider text-slate-500">
        {title}
      </h4>
      {children}
    </section>
  )
}

function Empty({ text }: { text: string }) {
  return <p className="text-[12px] italic text-slate-600">{text}</p>
}

function StringList({ items, mono = true }: { items?: string[]; mono?: boolean }) {
  if (!items || items.length === 0) return <Empty text="（无）" />
  return (
    <ul className="space-y-1">
      {items.map((s, i) => (
        <li
          key={`${i}-${s}`}
          className={`break-all rounded bg-ink-950/50 px-2 py-1 text-[11.5px] text-slate-300 ${
            mono ? 'font-mono' : ''
          }`}
        >
          {s}
        </li>
      ))}
    </ul>
  )
}

function ContractView({ c }: { c: ContractSpec }) {
  return (
    <div className="space-y-2.5">
      <div>
        <p className="mb-1 text-[11px] text-slate-500">输入</p>
        <StringList items={c.inputs} />
      </div>
      <div>
        <p className="mb-1 text-[11px] text-slate-500">产出</p>
        <StringList items={c.outputs} />
      </div>
      <div>
        <p className="mb-1 text-[11px] text-slate-500">依赖节点</p>
        <StringList items={c.dependencies} />
      </div>
      <div>
        <p className="mb-1 text-[11px] text-slate-500">约束</p>
        <StringList items={c.constraints} mono={false} />
      </div>
    </div>
  )
}

function DoDView({ dod }: { dod: DoD }) {
  return (
    <div className="space-y-2.5">
      {dod.description && <p className="text-[12px] text-slate-300">{dod.description}</p>}
      <div>
        <p className="mb-1 text-[11px] text-slate-500">验收命令</p>
        <StringList items={dod.commands} />
      </div>
      {dod.expected_output && (
        <div>
          <p className="mb-1 text-[11px] text-slate-500">期望输出包含</p>
          <p className="rounded bg-ink-950/50 px-2 py-1 font-mono text-[11.5px] text-slate-300">
            {dod.expected_output}
          </p>
        </div>
      )}
      <p className="text-[11px] text-slate-600">
        超时 {dod.timeout_sec || 60}s
      </p>
    </div>
  )
}

function Metric({ label, value }: { label: string; value: string }) {
  return (
    <div className="rounded-md border border-line bg-ink-950/40 px-2.5 py-1.5">
      <p className="text-[10px] uppercase tracking-wider text-slate-600">{label}</p>
      <p className="mt-0.5 font-mono text-[12px] text-slate-200">{value}</p>
    </div>
  )
}

export function NodeDetail({ node }: { node: TaskNode }) {
  const meta = stateMeta[node.state]
  const u = node.token_usage
  const terminal = node.state === 'COMPLETED' || node.state === 'FAILED'

  return (
    <div className="h-full overflow-y-auto">
      <div className="border-b border-line px-4 py-3">
        <div className="flex items-center gap-2">
          <span className={`rounded border px-1.5 py-0.5 text-[10.5px] ${meta.ring} ${meta.color}`}>
            {meta.label}
          </span>
          <span className="rounded border border-line px-1.5 py-0.5 text-[10.5px] text-slate-400">
            {node.type === 'LEAF' ? '叶子' : '复合'} · depth {node.depth}
          </span>
        </div>
        <h3 className="mt-2 break-words text-[13.5px] font-semibold leading-snug text-slate-100">
          {node.title}
        </h3>
        <p className="mt-0.5 font-mono text-[10.5px] text-slate-600">{node.id}</p>
        {node.description && node.description !== node.title && (
          <p className="mt-2 whitespace-pre-wrap break-words text-[12px] leading-relaxed text-slate-400">
            {node.description}
          </p>
        )}
      </div>

      <Section title="指标">
        <div className="grid grid-cols-2 gap-2">
          <Metric label="重试" value={String(node.retry_count ?? 0)} />
          <Metric label="模型调用" value={String(u?.calls ?? 0)} />
          <Metric label="Token" value={compactTokens(u?.total_tokens ?? 0)} />
          <Metric
            label="耗时"
            value={formatDuration(node.created_at, terminal ? node.updated_at : undefined) || '—'}
          />
          <Metric
            label="prompt"
            value={compactTokens(u?.prompt_tokens ?? 0)}
          />
          <Metric
            label="completion"
            value={compactTokens(u?.completion_tokens ?? 0)}
          />
        </div>
        {node.integration_verified && (
          <p className="mt-2 flex items-center gap-1.5 text-[11.5px] text-emerald-300">
            <PhaseBadge phase="done" />
            目标级端到端验收已通过
          </p>
        )}
      </Section>

      <Section title="契约">
        <ContractView c={node.contract ?? { inputs: [], outputs: [], dependencies: [], constraints: [] }} />
      </Section>

      <Section title="验收标准 (DoD)">
        <DoDView dod={node.dod ?? {}} />
      </Section>

      {node.result_summary && (
        <Section title="结果摘要">
          <p className="whitespace-pre-wrap break-words rounded bg-ink-950/50 px-2 py-1.5 font-mono text-[11.5px] leading-relaxed text-slate-300">
            {node.result_summary}
          </p>
        </Section>
      )}

      <Section title={`错误历史${node.error_history?.length ? ` (${node.error_history.length})` : ''}`}>
        {node.error_history && node.error_history.length > 0 ? (
          <ol className="space-y-2">
            {[...node.error_history].reverse().map((e, i) => (
              <li key={`${e.time}-${i}`} className="rounded-md border border-rose-900/40 bg-rose-950/20 px-2.5 py-1.5">
                <p className="font-mono text-[10px] text-slate-500">
                  {new Date(e.time).toLocaleString()}
                  {e.fingerprint && <span title={e.fingerprint}> · fp {e.fingerprint.slice(0, 8)}</span>}
                </p>
                <p className="mt-0.5 whitespace-pre-wrap break-words text-[11.5px] leading-relaxed text-rose-200/90">
                  {e.error}
                </p>
              </li>
            ))}
          </ol>
        ) : (
          <Empty text="暂无失败记录" />
        )}
      </Section>
    </div>
  )
}
