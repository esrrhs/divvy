import { useState, type FormEvent } from 'react'
import { useStore } from '../store'
import type { ApprovalMode } from '../types'
import { SpinnerIcon } from './icons'

interface FormState {
  goal: string
  model: string
  baseURL: string
  apiKey: string
  parallel: string
  isolate: boolean
  gitCommit: boolean
  web: boolean
  browser: boolean
  maxCost: string
  budgetTokens: string
  approvalMode: ApprovalMode
}

const initial: FormState = {
  goal: '',
  model: 'gpt-4o-mini',
  baseURL: 'https://api.openai.com/v1',
  apiKey: '',
  parallel: '1',
  isolate: true,
  gitCommit: false,
  web: false,
  browser: false,
  maxCost: '',
  budgetTokens: '',
  approvalMode: 'auto',
}

function Toggle({
  checked,
  onChange,
  label,
  hint,
}: {
  checked: boolean
  onChange: (v: boolean) => void
  label: string
  hint?: string
}) {
  return (
    <label className="flex cursor-pointer items-start gap-2.5 py-1">
      <button
        type="button"
        role="switch"
        aria-checked={checked}
        onClick={() => onChange(!checked)}
        className={`mt-0.5 h-4 w-7 shrink-0 rounded-full transition ${
          checked ? 'bg-brand-600' : 'bg-ink-600'
        }`}
      >
        <span
          className={`block h-3 w-3 translate-y-0.5 rounded-full bg-white transition-transform ${
            checked ? 'translate-x-3.5' : 'translate-x-0.5'
          }`}
        />
      </button>
      <span>
        <span className="block text-[12.5px] text-slate-300">{label}</span>
        {hint && <span className="block text-[11px] leading-snug text-slate-500">{hint}</span>}
      </span>
    </label>
  )
}

export function NewSessionForm({ onCreated }: { onCreated?: (id: string) => void }) {
  const createSession = useStore((s) => s.createSession)
  const [f, setF] = useState<FormState>(initial)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const set = <K extends keyof FormState>(k: K, v: FormState[K]) =>
    setF((prev) => ({ ...prev, [k]: v }))

  async function submit(e: FormEvent) {
    e.preventDefault()
    if (!f.goal.trim() || busy) return
    setBusy(true)
    setError(null)
    try {
      const id = await createSession({
        goal: f.goal.trim(),
        model: f.model.trim() || undefined,
        base_url: f.baseURL.trim() || undefined,
        api_key: f.apiKey.trim() || undefined,
        parallel: Number(f.parallel) || 1,
        isolate: f.isolate,
        git_commit: f.gitCommit,
        web: f.web,
        browser: f.browser,
        max_cost: f.maxCost ? Number(f.maxCost) : undefined,
        budget_tokens: f.budgetTokens ? Number(f.budgetTokens) : undefined,
        approval_mode: f.approvalMode,
      })
      setF(initial)
      onCreated?.(id)
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err))
    } finally {
      setBusy(false)
    }
  }

  return (
    <form onSubmit={submit} className="space-y-5">
      <div>
        <label className="label" htmlFor="goal">
          目标 *
        </label>
        <textarea
          id="goal"
          className="input min-h-[96px] resize-y leading-relaxed"
          placeholder="描述要 divvy 完成的开发目标，例如：用 Go 写一个带 /health 的 HTTP 服务并附带单测"
          value={f.goal}
          onChange={(e) => set('goal', e.target.value)}
        />
      </div>

      <div className="grid grid-cols-2 gap-3">
        <div>
          <label className="label" htmlFor="model">
            模型
          </label>
          <input
            id="model"
            className="input"
            value={f.model}
            onChange={(e) => set('model', e.target.value)}
            placeholder="gpt-4o-mini"
          />
        </div>
        <div>
          <label className="label" htmlFor="parallel">
            并行叶子数
          </label>
          <input
            id="parallel"
            type="number"
            min={1}
            max={8}
            className="input"
            value={f.parallel}
            onChange={(e) => set('parallel', e.target.value)}
          />
        </div>
      </div>

      <div>
        <label className="label" htmlFor="baseurl">
          Base URL
        </label>
        <input
          id="baseurl"
          className="input font-mono text-[12px]"
          value={f.baseURL}
          onChange={(e) => set('baseURL', e.target.value)}
          placeholder="https://api.openai.com/v1 或 http://127.0.0.1:11434/v1"
        />
      </div>

      <div>
        <label className="label" htmlFor="apikey">
          API Key（仅存内存，不写入日志/事件流）
        </label>
        <input
          id="apikey"
          type="password"
          className="input font-mono text-[12px]"
          value={f.apiKey}
          onChange={(e) => set('apiKey', e.target.value)}
          placeholder="sk-...（本地模型可留空）"
          autoComplete="off"
        />
      </div>

      <div className="grid grid-cols-2 gap-3">
        <div>
          <label className="label" htmlFor="maxcost">
            成本上限 (USD)
          </label>
          <input
            id="maxcost"
            type="number"
            min={0}
            step="0.01"
            className="input"
            value={f.maxCost}
            onChange={(e) => set('maxCost', e.target.value)}
            placeholder="0 = 不限"
          />
        </div>
        <div>
          <label className="label" htmlFor="budget">
            Token 预算
          </label>
          <input
            id="budget"
            type="number"
            min={0}
            step="1000"
            className="input"
            value={f.budgetTokens}
            onChange={(e) => set('budgetTokens', e.target.value)}
            placeholder="0 = 不限"
          />
        </div>
      </div>

      <div>
        <span className="label">叶子改动审批</span>
        <div className="grid grid-cols-2 gap-2">
          {(
            [
              ['auto', '自动（不中断）'],
              ['manual', '人工审批 diff'],
            ] as const
          ).map(([v, label]) => (
            <button
              key={v}
              type="button"
              onClick={() => set('approvalMode', v)}
              className={`rounded-md border px-3 py-1.5 text-[12.5px] transition ${
                f.approvalMode === v
                  ? 'border-brand-600 bg-brand-600/15 text-brand-400'
                  : 'border-line bg-ink-950/50 text-slate-400 hover:border-ink-500'
              }`}
            >
              {label}
            </button>
          ))}
        </div>
      </div>

      <div className="grid grid-cols-2 gap-x-4 rounded-lg border border-line bg-ink-950/40 px-3 py-1.5">
        <Toggle
          label="隔离执行 (-isolate)"
          hint="叶子在工作区副本中运行，验证通过才合并；manual 审批必需"
          checked={f.isolate}
          onChange={(v) => set('isolate', v)}
        />
        <Toggle
          label="每个叶子提交 git"
          hint="-git-commit，工作区需为 git 仓库"
          checked={f.gitCommit}
          onChange={(v) => set('gitCommit', v)}
        />
        <Toggle
          label="允许联网搜索"
          hint="web_search / web_fetch"
          checked={f.web}
          onChange={(v) => set('web', v)}
        />
        <Toggle
          label="允许无头浏览器"
          hint="需要本机 Chrome/Chromium"
          checked={f.browser}
          onChange={(v) => set('browser', v)}
        />
      </div>

      {error && (
        <div className="rounded-md border border-rose-800/60 bg-rose-950/40 px-3 py-2 text-[12.5px] text-rose-300">
          {error}
        </div>
      )}

      <button type="submit" disabled={busy || !f.goal.trim()} className="btn-primary w-full py-2">
        {busy && <SpinnerIcon width={14} height={14} />}
        {busy ? '正在创建…' : '创建会话'}
      </button>
    </form>
  )
}
