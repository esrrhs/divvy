import { useEffect } from 'react'
import { useStore } from '../store'
import { XIcon } from './icons'

// Transient error toast for control actions (approve/pause/add/redo/answer).
// Auto-dismisses after 6s; also cleared by the next successful action.
export function ActionToast() {
  const error = useStore((s) => s.actionError)
  const clear = useStore((s) => s.clearActionError)

  useEffect(() => {
    if (!error) return
    const t = setTimeout(clear, 6000)
    return () => clearTimeout(t)
  }, [error, clear])

  if (!error) return null
  return (
    <div className="fixed bottom-4 right-4 z-50 flex max-w-[380px] items-start gap-2 rounded-lg border border-rose-800/70 bg-rose-950/90 px-3 py-2.5 text-[12.5px] text-rose-200 shadow-2xl">
      <span className="break-words leading-snug">{error}</span>
      <button onClick={clear} className="shrink-0 text-rose-400 hover:text-rose-200" aria-label="关闭">
        <XIcon width={13} height={13} />
      </button>
    </div>
  )
}
