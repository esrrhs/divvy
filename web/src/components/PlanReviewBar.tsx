import { useState } from 'react'
import { useStore } from '../store'
import { CheckIcon, SpinnerIcon, XIcon } from './icons'

export function PlanReviewBar() {
  const planApprove = useStore((s) => s.planApprove)
  const planAdjust = useStore((s) => s.planAdjust)
  const abort = useStore((s) => s.abort)
  const [adjusting, setAdjusting] = useState(false)
  const [comment, setComment] = useState('')
  const [busy, setBusy] = useState(false)

  async function run(fn: () => Promise<boolean>, after?: () => void) {
    setBusy(true)
    const ok = await fn()
    setBusy(false)
    if (ok) {
      setComment('')
      setAdjusting(false)
      after?.()
    }
  }

  return (
    <div className="shrink-0 border-b border-amber-800/50 bg-amber-950/25 px-5 py-3">
      <div className="flex items-center gap-3">
        <span className="flex h-2 w-2 shrink-0 animate-pulse rounded-full bg-amber-400" />
        <p className="text-[13px] font-medium text-amber-200">
          计划已就绪：批准后开始执行，或填写调整意见让 divvy 重新规划
        </p>
        <div className="ml-auto flex shrink-0 items-center gap-2">
          {!adjusting && (
            <>
              <button
                className="btn-ghost border-rose-800/60 text-rose-300 hover:bg-rose-950/40"
                disabled={busy}
                onClick={() => void run(abort)}
                title="中止：计划保存但不执行"
              >
                <XIcon width={13} height={13} />
                中止
              </button>
              <button
                className="btn-ghost"
                disabled={busy}
                onClick={() => setAdjusting(true)}
              >
                调整
              </button>
              <button
                className="btn-primary"
                disabled={busy}
                onClick={() => void run(planApprove)}
              >
                {busy ? <SpinnerIcon width={13} height={13} /> : <CheckIcon width={13} height={13} />}
                批准执行
              </button>
            </>
          )}
        </div>
      </div>

      {adjusting && (
        <div className="mt-3 flex items-start gap-2">
          <textarea
            className="input min-h-[64px] flex-1 resize-y text-[12.5px]"
            placeholder="例如：把存储层和 API 层拆成两个叶子；再加一个命令行入口"
            value={comment}
            onChange={(e) => setComment(e.target.value)}
            autoFocus
          />
          <div className="flex shrink-0 flex-col gap-2">
            <button
              className="btn-primary px-3 py-1.5"
              disabled={busy || !comment.trim()}
              onClick={() => void run(() => planAdjust(comment))}
            >
              {busy && <SpinnerIcon width={13} height={13} />}
              提交意见
            </button>
            <button
              className="btn-ghost px-3 py-1"
              disabled={busy}
              onClick={() => {
                setAdjusting(false)
                setComment('')
              }}
            >
              取消
            </button>
          </div>
        </div>
      )}
    </div>
  )
}
