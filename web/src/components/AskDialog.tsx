import { useEffect, useState } from 'react'
import { useStore } from '../store'
import { SpinnerIcon } from './icons'

// FR-7: the leaf called ask. A modal answer box blocks the session view;
// answering "暂停" is supported and stops the run (same semantics as the
// terminal guider).
export function AskDialog() {
  const question = useStore((s) => s.live?.pending_ask ?? '')
  const answer = useStore((s) => s.answer)
  const pause = useStore((s) => s.pause)
  const actionError = useStore((s) => s.actionError)
  const [text, setText] = useState('')
  const [busy, setBusy] = useState(false)

  useEffect(() => {
    setText('')
  }, [question])

  if (!question) return null

  async function submit() {
    if (!text.trim()) return
    setBusy(true)
    // A literal pause answer ends the run instead of answering the worker.
    if (text.trim() === '暂停' || text.trim().toLowerCase() === '/pause') {
      await pause()
    } else {
      await answer(text)
    }
    setBusy(false)
  }

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/55 p-6 backdrop-blur-[1px]">
      <div className="w-full max-w-[520px] overflow-hidden rounded-xl border border-orange-800/50 bg-ink-850 shadow-2xl">
        <div className="flex items-center gap-2 border-b border-line bg-orange-950/30 px-4 py-2.5">
          <span className="h-2 w-2 animate-pulse rounded-full bg-orange-400" />
          <h3 className="text-[13px] font-semibold text-orange-200">叶子在向你提问</h3>
        </div>
        <div className="px-4 py-3">
          <p className="whitespace-pre-wrap break-words rounded-md border border-line bg-ink-950/50 px-3 py-2.5 text-[13px] leading-relaxed text-slate-200">
            {question}
          </p>
          <textarea
            className="input mt-3 min-h-[84px] resize-y"
            placeholder="输入回答后发送；输入「暂停」可保存任务树并停止运行"
            value={text}
            onChange={(e) => setText(e.target.value)}
            autoFocus
            onKeyDown={(e) => {
              if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) void submit()
            }}
          />
          {actionError && (
            <p className="mt-2 rounded border border-rose-800/60 bg-rose-950/40 px-2.5 py-1.5 text-[12px] text-rose-300">
              {actionError}
            </p>
          )}
          <p className="mt-1.5 text-[11px] text-slate-600">⌘/Ctrl + Enter 发送</p>
        </div>
        <div className="flex justify-end gap-2 border-t border-line bg-ink-900/50 px-4 py-2.5">
          <button
            className="btn-ghost"
            disabled={busy}
            onClick={() => void pause()}
            title="不回答，直接暂停保存"
          >
            暂停运行
          </button>
          <button className="btn-primary" disabled={busy || !text.trim()} onClick={() => void submit()}>
            {busy && <SpinnerIcon width={13} height={13} />}
            发送回答
          </button>
        </div>
      </div>
    </div>
  )
}
