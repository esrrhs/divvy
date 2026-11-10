// Pure helpers for the project-conversation UI. Kept free of React so they
// can be unit-tested without a DOM.

export type ComposeKind = 'start' | 'steer' | 'paused' | 'blocked'

export type ComposeState =
  | {
      ok: true
      kind: 'start'
      hint: string
      primary: string
      canTakeover: false
      canQueue: false
    }
  | {
      ok: true
      kind: 'steer'
      hint: string
      primary: string
      canTakeover: true
      canQueue: true
    }
  | {
      ok: true
      kind: 'paused'
      hint: string
      primary: string
      canTakeover: true
      canQueue: false
    }
  | { ok: false; kind: 'blocked'; reason: string }

const runningPhases = new Set([
  'planning',
  'plan_review',
  'running',
  'leaf_approval',
  'ask',
])

export function composeState(
  phase?: string,
  keyRequired?: boolean,
  agentMode?: string,
  pendingRollback?: { message?: string } | null | boolean,
): ComposeState {
  if (pendingRollback) {
    const msg =
      typeof pendingRollback === 'object' && pendingRollback && pendingRollback.message
        ? pendingRollback.message
        : '上次撤销未完成，已保留操作日志和备份。在恢复完成前不能开始新的写入，以免覆盖未完成的恢复。'
    return { ok: false, kind: 'blocked', reason: msg }
  }
  if (keyRequired) {
    return {
      ok: false,
      kind: 'blocked',
      reason: '当前模型需要 API key。服务重启后密钥不会保留，请先在本对话重新填写（不会写入历史）。',
    }
  }
  if (phase && runningPhases.has(phase)) {
    return {
      ok: true,
      kind: 'steer',
      hint:
        agentMode === 'direct'
          ? '当前直接 agent 循环还在进行。默认「插入到当前轮」：会在当前工具结束后的下一模型步骤注入同一循环，不会取消正在执行的工具，也不会丢掉本轮已完成的修改。想等本轮结束后再做另一件事，用「排队下一条」；要放弃当前轮并用新指令重来，用「中止并接管」。暂停/续跑仍然可用。'
          : '当前任务还在进行。默认「插入到当前轮」：执行中会立刻打断当前叶子，并把指令纳入接下来的步骤或重新规划（已完成的叶子保留；该叶子尚未合并的改动会丢弃）。计划审查阶段会立即重新规划。若正在叶子审批或问答，会先记下，等该步骤结束后再生效，不会绕过审批。想等整轮彻底结束后再做另一件事，用「排队下一条」；要放弃当前轮并用新指令重来，用「中止并接管」。审批和暂停/续跑不受影响。',
      primary: '插入到当前轮',
      canTakeover: true,
      canQueue: true,
    }
  }
  if (phase === 'paused') {
    return {
      ok: true,
      kind: 'paused',
      hint: '当前任务已暂停。请先「续跑」接着上次往下走；也可以先「插入到当前轮」（记下调整，续跑后再注入），或「中止并接管」用新指令重新开始。暂停期间不会自动开跑排队项。',
      primary: '续跑当前任务',
      canTakeover: true,
      canQueue: false,
    }
  }
  return {
    ok: true,
    kind: 'start',
    hint: '',
    primary: '发送并执行',
    canTakeover: false,
    canQueue: false,
  }
}

export function isResumablePhase(phase?: string): boolean {
  return phase === 'paused'
}

export function isRunningPhase(phase?: string): boolean {
  return !!phase && runningPhases.has(phase)
}

/** Find an @mention query immediately before the cursor. */
export function mentionQuery(text: string, cursor: number): { start: number; query: string } | null {
  const pos = Math.max(0, Math.min(cursor, text.length))
  const before = text.slice(0, pos)
  const at = before.lastIndexOf('@')
  if (at < 0) return null
  if (at > 0) {
    const prev = before[at - 1]
    if (prev && !/\s/.test(prev)) return null
  }
  const query = before.slice(at + 1)
  if (/[\s]/.test(query)) return null
  return { start: at, query }
}

export function outcomeMeta(outcome?: string): { label: string; cls: string } {
  switch (outcome) {
    case 'completed':
      return { label: '成功', cls: 'border-emerald-700/50 bg-emerald-950/40 text-emerald-300' }
    case 'failed':
      return { label: '失败', cls: 'border-rose-800/50 bg-rose-950/40 text-rose-300' }
    case 'paused':
      return { label: '已暂停', cls: 'border-amber-800/50 bg-amber-950/30 text-amber-300' }
    case 'aborted':
      return { label: '已中止', cls: 'border-slate-600/50 bg-ink-800 text-slate-300' }
    default:
      return { label: outcome || '结束', cls: 'border-line bg-ink-800 text-slate-400' }
  }
}

export function messageKindMeta(kind: string): { label: string; cls: string } {
  switch (kind) {
    case 'user':
      return { label: '你', cls: 'border-brand-700/40 bg-brand-950/30 text-slate-200' }
    case 'run_started':
      return { label: '执行状态', cls: 'border-line bg-ink-900/60 text-slate-400' }
    case 'run_finished':
      return { label: '执行结果', cls: 'border-line bg-ink-900/60 text-slate-300' }
    case 'need_key':
      return { label: '需要密钥', cls: 'border-amber-800/50 bg-amber-950/20 text-amber-200' }
    case 'model_reply':
      return { label: '模型回答', cls: 'border-brand-700/40 bg-brand-950/20 text-slate-100' }
    case 'tool_activity':
      return { label: '工具', cls: 'border-line bg-ink-950/70 font-mono text-[12px] text-slate-400' }
    case 'ask':
      return { label: '模型提问', cls: 'border-orange-800/50 bg-orange-950/20 text-orange-100' }
    case 'ask_answer':
      return { label: '你的回答', cls: 'border-brand-700/40 bg-brand-950/30 text-slate-200' }
    case 'notice':
    default:
      return { label: '说明', cls: 'border-line bg-ink-900/40 text-slate-400' }
  }
}

export function conversationRowKind(phase?: string): 'active' | 'paused' | 'done' | 'failed' | 'idle' {
  if (!phase) return 'idle'
  if (phase === 'paused') return 'paused'
  if (phase === 'done') return 'done'
  if (phase === 'failed') return 'failed'
  if (runningPhases.has(phase)) return 'active'
  return 'idle'
}

export function shortWorkdir(path: string): string {
  const p = path.replace(/\\/g, '/').replace(/\/+$/, '')
  const parts = p.split('/').filter(Boolean)
  if (parts.length <= 2) return p || path
  return '.../' + parts.slice(-2).join('/')
}

export function sameRelPath(a?: string, b?: string): boolean {
  const norm = (s?: string) =>
    (s ?? '')
      .replace(/\\/g, '/')
      .replace(/^\.\//, '')
      .replace(/^\/+/, '')
      .replace(/\/+$/, '')
  return norm(a) !== '' && norm(a) === norm(b)
}

export function appliedStatusLabel(status?: string): string {
  switch (status) {
    case 'accepted':
      return '已全部接受'
    case 'rejected':
      return '已全部拒绝'
    case 'partial':
      return '已部分接受'
    case 'rolled_back':
      return '已随轮次撤销'
    default:
      return status ? '已按块更新' : ''
  }
}

/** Default unspecified hunks to accept, matching the server. */
export function initialHunkAccepts(hunkCount: number, stored?: string[]): boolean[] {
  const out = Array.from({ length: Math.max(0, hunkCount) }, () => true)
  if (!stored) return out
  stored.forEach((s, i) => {
    if (i >= out.length) return
    const v = s.toLowerCase()
    out[i] = v === 'accept' || v === 'accepted' || v === 'true'
  })
  return out
}

export function decisionsFromAccepts(accepts: boolean[]): { index: number; accept: boolean }[] {
  return accepts.map((accept, index) => ({ index, accept }))
}

export const steerPendingBanner =
  '待注入当前轮：当前处于计划审查等待之外的审批、问答或暂停，会在该步骤结束后注入（不会绕过审批，已完成工作保留）。执行中的插入会马上打断当前叶子。'

export function steerPendingText(agentMode?: string, inTool?: boolean): string {
  if (agentMode === 'direct') {
    return inTool
      ? '待注入当前循环：当前工具结束后的下一模型步骤生效（不会取消正在执行的工具，已完成修改保留）。'
      : '待注入当前循环：将在下一模型步骤生效（不会取消本轮或丢掉已完成的修改）。'
  }
  return steerPendingBanner
}

export interface CodeSpan {
  start_line: number
  end_line: number
}

/** 1-based line range covered by a character offset selection in `content`. */
export function lineRangeFromOffsets(content: string, start: number, end: number): CodeSpan {
  const from = Math.max(0, Math.min(start, end, content.length))
  let to = Math.max(0, Math.min(Math.max(start, end), content.length))
  if (to > from && content[to - 1] === '\n') {
    to -= 1
  }
  const startLine = content.slice(0, from).split('\n').length
  const endLine = content.slice(0, to).split('\n').length
  return { start_line: startLine, end_line: Math.max(startLine, endLine) }
}

export function selectionLabel(sel: { path: string; start_line: number; end_line: number; draft?: boolean }): string {
  const lines =
    sel.start_line === sel.end_line ? `L${sel.start_line}` : `L${sel.start_line}-${sel.end_line}`
  return `${sel.path}:${lines}${sel.draft ? '（未保存草稿）' : ''}`
}
