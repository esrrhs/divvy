import { useEffect, useState } from 'react'
import { api, ApiError } from '../api/client'
import { useStore } from '../store'
import type { ApprovalPayload } from '../types'
import { FileDiffList } from './FileDiffView'
import { CheckIcon, SpinnerIcon, XIcon } from './icons'

// Shown when the run is parked in leaf_approval. Fetches the diff payload
// for the leaf named in the phase snapshot; supports the full
// reject → redo → re-approve loop (the card stays mounted across rounds).
export function LeafApprovalCard() {
  const sessionId = useStore((s) => s.currentId)
  const phase = useStore((s) => s.live?.phase)
  const leafId = useStore((s) => s.live?.leaf_approving ?? '')
  const decideLeaf = useStore((s) => s.decideLeaf)
  const actionError = useStore((s) => s.actionError)
  const clearActionError = useStore((s) => s.clearActionError)

  const [payload, setPayload] = useState<ApprovalPayload | null>(null)
  const [loadError, setLoadError] = useState<string | null>(null)
  const [comment, setComment] = useState('')
  const [showReject, setShowReject] = useState(false)
  const [busy, setBusy] = useState(false)

  useEffect(() => {
    setPayload(null)
    setLoadError(null)
    setComment('')
    setShowReject(false)
    if (!sessionId || !leafId || phase !== 'leaf_approval') return
    let cancelled = false
    // The leaf hook registers its payload just before the phase flips;
    // retry briefly to cover that tiny window.
    const tries = [0, 150, 400]
    let timers: ReturnType<typeof setTimeout>[] = []
    const load = (n: number) => {
      api
        .getApproval(sessionId, leafId)
        .then((p) => {
          if (!cancelled) setPayload(p)
        })
        .catch((e) => {
          if (cancelled) return
          if (e instanceof ApiError && e.status === 404 && n < tries.length - 1) {
            timers.push(setTimeout(() => load(n + 1), tries[n + 1] - tries[n]))
          } else if (n === tries.length - 1) {
            setLoadError(e instanceof Error ? e.message : String(e))
          }
        })
    }
    load(0)
    return () => {
      cancelled = true
      timers.forEach(clearTimeout)
    }
  }, [sessionId, leafId, phase])

  async function decide(decision: 'approve' | 'reject') {
    if (!leafId) return
    setBusy(true)
    const ok = await decideLeaf(leafId, decision, comment)
    setBusy(false)
    if (ok) {
      setComment('')
      setShowReject(false)
      // The next approval round (after a rejection) triggers a refetch via
      // the leafId effect even if the id is unchanged: payload itself is
      // invalidated by the phase leaving/returning leaf_approval.
      setPayload(null)
    }
  }

  return (
    <div className="shrink-0 border-b border-fuchsia-800/50 bg-fuchsia-950/20">
      <div className="flex items-center gap-3 px-5 pt-3">
        <span className="flex h-2 w-2 shrink-0 animate-pulse rounded-full bg-fuchsia-400" />
        <p className="text-[13px] font-medium text-fuchsia-200">
          叶子改动审查{payload ? `：${payload.title}` : ''}
          <span className="ml-2 font-mono text-[11px] text-fuchsia-400/70">{leafId}</span>
        </p>
        <span className="ml-auto rounded border border-fuchsia-800/60 px-1.5 py-px text-[10.5px] text-fuchsia-300">
          批准后才会合并到工作区
        </span>
      </div>

      <div className="max-h-[42vh] overflow-y-auto px-5 py-3">
        {loadError ? (
          <p className="text-[12.5px] text-rose-300">无法加载 diff：{loadError}</p>
        ) : !payload ? (
          <p className="flex items-center gap-2 py-2 text-[12.5px] text-slate-400">
            <SpinnerIcon width={13} height={13} />
            正在生成变更清单…
          </p>
        ) : (
          <FileDiffList changes={payload.changes} />
        )}
      </div>

      <div className="border-t border-fuchsia-900/40 px-5 py-3">
        {showReject && (
          <textarea
            className="input mb-2 min-h-[56px] resize-y text-[12.5px]"
            placeholder="拒绝原因（必填）：会作为 prevError 反馈给叶子，要求它按意见重做"
            value={comment}
            onChange={(e) => {
              setComment(e.target.value)
              if (actionError) clearActionError()
            }}
            autoFocus
          />
        )}
        {actionError && (
          <p className="mb-2 rounded border border-rose-800/60 bg-rose-950/40 px-2.5 py-1.5 text-[12px] text-rose-300">
            {actionError}
          </p>
        )}
        <div className="flex items-center gap-2">
          {showReject ? (
            <>
              <button
                className="btn flex-1 border-rose-700 bg-rose-800/70 text-white hover:bg-rose-700"
                disabled={busy || !comment.trim() || !payload}
                onClick={() => void decide('reject')}
              >
                {busy ? <SpinnerIcon width={13} height={13} /> : <XIcon width={13} height={13} />}
                拒绝并要求重做
              </button>
              <button
                className="btn-ghost"
                disabled={busy}
                onClick={() => {
                  setShowReject(false)
                  setComment('')
                }}
              >
                返回
              </button>
            </>
          ) : (
            <>
              <button
                className="btn flex-1 border-rose-800/70 text-rose-300 hover:bg-rose-950/40"
                disabled={busy || !payload}
                onClick={() => setShowReject(true)}
              >
                <XIcon width={13} height={13} />
                拒绝（带意见重做）
              </button>
              <button
                className="btn-primary flex-1"
                disabled={busy || !payload}
                onClick={() => void decide('approve')}
              >
                {busy ? <SpinnerIcon width={13} height={13} /> : <CheckIcon width={13} height={13} />}
                批准合并
              </button>
            </>
          )}
        </div>
      </div>
    </div>
  )
}
