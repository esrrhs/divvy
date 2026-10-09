import type { StreamEvent } from '../types'

const kindMeta: Record<
  string,
  { label: string; dot: string }
> = {
  plan_review: { label: '计划评审', dot: 'bg-amber-400' },
  state_change: { label: '状态变更', dot: 'bg-brand-400' },
  tree_snapshot: { label: '树快照', dot: 'bg-slate-500' },
  leaf_approval: { label: '叶子审批', dot: 'bg-fuchsia-400' },
  ask_pending: { label: '等待回答', dot: 'bg-orange-400' },
  ask: { label: '问答', dot: 'bg-orange-400' },
  retry: { label: '重试', dot: 'bg-amber-500' },
  verify: { label: '验收', dot: 'bg-cyan-400' },
  llm_call: { label: '模型调用', dot: 'bg-violet-400' },
  tool_call: { label: '工具调用', dot: 'bg-teal-400' },
  finish_gate: { label: '收尾闸门', dot: 'bg-sky-400' },
  user_pause: { label: '暂停', dot: 'bg-slate-500' },
  leaf_stall: { label: '停滞', dot: 'bg-rose-500' },
  leaf_timeout: { label: '超时', dot: 'bg-rose-500' },
  leaf_giveup: { label: '放弃', dot: 'bg-rose-500' },
  session_start: { label: '会话开始', dot: 'bg-slate-400' },
  session_end: { label: '会话结束', dot: 'bg-slate-400' },
}

function summary(ev: StreamEvent): string {
  switch (ev.kind) {
    case 'state_change':
      return `${ev.from ?? '—'} → ${ev.to ?? '—'}${ev.error ? `：${ev.error}` : ''}`
    case 'plan_review':
      if (ev.phase === 'pending') return '计划就绪，等待批准/调整/中止'
      if (ev.decision === 'approved' || ev.phase === 'approved') return '计划已批准，开始执行'
      if (ev.decision === 'aborted') return '计划被中止（已保存，未执行）'
      if (ev.decision === 'adjusted' || ev.phase === 'adjusted')
        return `按意见重新规划：${ev.feedback ?? ''}`
      return String(ev.phase ?? '')
    case 'leaf_approval':
      return ev.phase === 'pending'
        ? `${ev.files ?? '?'} 个文件待审查`
        : ev.phase === 'approved'
          ? '改动已批准，合并发布'
          : `已拒绝：${ev.comment ?? ''}`
    case 'ask_pending':
      return String(ev.question ?? '')
    case 'ask':
      return `Q: ${ev.question ?? ''}\nA: ${ev.answer ?? ''}`
    case 'verify':
      return ev.ok === false
        ? `未通过 (exit ${ev.exit_code ?? '?'})：${String(ev.command ?? '').slice(0, 80)} — ${String(ev.error ?? '').slice(0, 200) || (ev.timed_out ? '超时' : '非零退出')}`
        : `通过：${String(ev.command ?? '').slice(0, 120)}`
    case 'tool_call':
      return `${ev.tool ?? ''}${ev.args ? ` ${String(ev.args).slice(0, 100)}` : ''}${
        ev.ok === false ? ` 失败：${ev.error}` : ''
      }`
    case 'llm_call':
      return `${ev.call_kind ? String(ev.call_kind) + ' · ' : ''}${ev.model ?? ''}${
        ev.total_tokens ? ` · ${ev.total_tokens} tok` : ''
      }${ev.resp_tool_calls ? ` → ${(ev.resp_tool_calls as string[]).join(', ')}` : ''}`
    case 'retry':
      return `attempt ${ev.attempt ?? ''}：${String(ev.error ?? ev.reason ?? '').slice(0, 200)}`
    case 'finish_gate':
      return String(ev.reason ?? ev.phase ?? '')
    case 'leaf_stall':
    case 'leaf_timeout':
    case 'leaf_giveup':
      return String(ev.reason ?? ev.error ?? '')
    case 'session_end':
      return `结果：${ev.outcome ?? ''}${ev.error ? `：${ev.error}` : ''}`
    default:
      return ev.reason || ev.error ? String(ev.reason ?? ev.error) : ''
  }
}

const displayKinds = new Set(Object.keys(kindMeta))

export function EventTimeline({ events }: { events: StreamEvent[] }) {
  const shown = events.filter((e) => displayKinds.has(e.kind)).slice().reverse()

  if (shown.length === 0) {
    return (
      <div className="flex flex-1 items-center justify-center text-[12.5px] text-slate-600">
        暂无事件，等待运行开始…
      </div>
    )
  }

  return (
    <div className="flex-1 overflow-y-auto px-5 py-3">
      <ol className="relative space-y-2 border-l border-line pl-4">
        {shown.slice(0, 500).map((ev, i) => {
          const meta = kindMeta[ev.kind] ?? { label: ev.kind, dot: 'bg-slate-600' }
          const text = summary(ev)
          return (
            <li key={`${ev.seq ?? 'x'}-${i}`} className="relative">
              <span
                className={`absolute -left-[21.5px] top-1.5 h-2 w-2 rounded-full border-2 border-ink-900 ${meta.dot}`}
              />
              <div className="flex items-baseline gap-2">
                <span className="text-[11.5px] font-medium text-slate-300">{meta.label}</span>
                {ev.node_id && (
                  <span className="font-mono text-[10px] text-slate-600">{String(ev.node_id)}</span>
                )}
                <span className="ml-auto shrink-0 font-mono text-[10px] text-slate-700">
                  {typeof ev.seq === 'number' ? `#${ev.seq}` : ''}
                </span>
              </div>
              {text && (
                <p className="mt-0.5 whitespace-pre-wrap break-words text-[11.5px] leading-relaxed text-slate-500">
                  {text}
                </p>
              )}
            </li>
          )
        })}
      </ol>
      {shown.length > 500 && (
        <p className="py-2 text-center text-[11px] text-slate-600">
          仅展示最近 500 条（本视图共 {shown.length} 条，磁盘事件流完整保留）
        </p>
      )}
    </div>
  )
}
