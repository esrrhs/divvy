import { useCallback, useEffect, useState } from 'react'
import { api } from '../api/client'
import { SpinnerIcon } from './icons'

const PAGE = 500

// Paged, tail-first session log viewer (GET /sessions/{id}/log). Starts at
// the last PAGE lines and loads older pages on demand; a refresh button
// pulls new lines from the current tail.
export function LogView({ sessionId }: { sessionId: string }) {
  const [blocks, setBlocks] = useState<Array<{ offset: number; lines: string[] }>>([])
  const [total, setTotal] = useState(0)
  const [tailOffset, setTailOffset] = useState<number | null>(null) // oldest loaded offset
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)

  const loadTail = useCallback(async () => {
    setLoading(true)
    setError(null)
    try {
      const first = await api.log(sessionId, 0, 1)
      const start = Math.max(0, first.total - PAGE)
      const page = await api.log(sessionId, start, PAGE)
      setBlocks([{ offset: start, lines: page.lines }])
      setTotal(page.total)
      setTailOffset(start)
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setLoading(false)
    }
  }, [sessionId])

  useEffect(() => {
    void loadTail()
  }, [loadTail])

  async function loadOlder() {
    if (tailOffset === null || tailOffset <= 0) return
    const start = Math.max(0, tailOffset - PAGE)
    const page = await api.log(sessionId, start, tailOffset - start)
    setBlocks((b) => [{ offset: start, lines: page.lines }, ...b])
    setTailOffset(start)
  }

  async function refreshNewer() {
    const page = await api.log(sessionId, 0, 1)
    setTotal(page.total)
    const lastEnd = blocks.length
      ? blocks[blocks.length - 1].offset + blocks[blocks.length - 1].lines.length
      : 0
    if (page.total > lastEnd) {
      const newer = await api.log(sessionId, lastEnd, page.total - lastEnd)
      setBlocks((b) => [...b, { offset: lastEnd, lines: newer.lines }])
    }
  }

  const lineCount = blocks.reduce((n, b) => n + b.lines.length, 0)

  return (
    <div className="flex h-full flex-col">
      <div className="flex shrink-0 items-center gap-2 border-b border-line px-3 py-1.5 text-[11px] text-slate-500">
        <span className="font-mono">{lineCount}/{total} 行</span>
        <button
          onClick={() => void loadOlder()}
          disabled={tailOffset === null || tailOffset <= 0}
          className="ml-auto rounded border border-line px-1.5 py-0.5 text-[11px] text-slate-400 hover:bg-ink-800 disabled:opacity-40"
        >
          更早
        </button>
        <button
          onClick={() => void refreshNewer()}
          className="rounded border border-line px-1.5 py-0.5 text-[11px] text-slate-400 hover:bg-ink-800"
        >
          刷新
        </button>
      </div>
      <div className="min-h-0 flex-1 overflow-auto px-2 py-2">
        {loading ? (
          <p className="flex items-center gap-2 px-2 py-3 text-[12px] text-slate-500">
            <SpinnerIcon width={12} height={12} />
            读取日志…
          </p>
        ) : error ? (
          <p className="rounded border border-rose-900/50 bg-rose-950/30 px-2 py-1.5 text-[11.5px] text-rose-300">
            {error}
          </p>
        ) : lineCount === 0 ? (
          <p className="px-2 py-4 text-center text-[12px] text-slate-600">暂无日志行。</p>
        ) : (
          <pre className="whitespace-pre-wrap break-all font-mono text-[11px] leading-[1.5] text-slate-400">
            {blocks.flatMap((b) => b.lines).join('\n')}
          </pre>
        )}
      </div>
    </div>
  )
}
