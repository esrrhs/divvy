import { useCallback, useEffect, useState } from 'react'
import { api } from '../api/client'
import { useStore } from '../store'
import type { FsEntry } from '../types'
import { DocIcon, FolderIcon, SpinnerIcon } from './icons'

// Breadcrumb segments: "" (root), "a", "a/b".
function crumbs(relPath: string): Array<{ name: string; path: string }> {
  const p = relPath.replace(/^\.\//, '').replace(/^\/+/, '')
  const parts = p ? p.split('/').filter(Boolean) : []
  const out: Array<{ name: string; path: string }> = [{ name: '工作区', path: '.' }]
  let acc = ''
  for (const part of parts) {
    acc = acc ? `${acc}/${part}` : part
    out.push({ name: part, path: acc })
  }
  return out
}

function humanSize(n: number): string {
  if (n < 1024) return `${n} B`
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`
  return `${(n / 1024 / 1024).toFixed(1)} MB`
}

export function FileTree() {
  const currentId = useStore((s) => s.currentId)
  const openFile = useStore((s) => s.openFile)
  const viewingPath = useStore((s) => s.fileView?.path)
  const [path, setPath] = useState('.')
  const [entries, setEntries] = useState<FsEntry[]>([])
  const [truncated, setTruncated] = useState(false)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)

  const load = useCallback(
    async (p: string) => {
      setLoading(true)
      setError(null)
      try {
        const res = await api.fsList(p, currentId ?? undefined)
        setEntries(res.entries)
        setTruncated(res.truncated)
        setPath(res.path || p)
      } catch (e) {
        setError(e instanceof Error ? e.message : String(e))
      } finally {
        setLoading(false)
      }
    },
    [currentId],
  )

  useEffect(() => {
    void load('.')
  }, [load])

  return (
    <div className="flex min-h-0 flex-1 flex-col">
      <div className="flex flex-wrap items-center gap-1 border-b border-line px-3 py-1.5 text-[11px]">
        {crumbs(path).map((c, i, arr) => (
          <span key={c.path} className="flex items-center gap-1">
            {i > 0 && <span className="text-slate-700">/</span>}
            <button
              onClick={() => void load(c.path)}
              className={`rounded px-1 py-0.5 text-slate-400 hover:bg-ink-800 hover:text-slate-200 ${
                i === arr.length - 1 ? 'text-slate-200' : ''
              }`}
              title={c.path === '.' ? '工作区根目录' : c.path}
            >
              {c.name}
            </button>
          </span>
        ))}
      </div>

      <div className="min-h-0 flex-1 overflow-y-auto px-2 py-2">
        {!currentId && (
          <p className="px-1.5 pb-2 text-[11px] leading-snug text-slate-600">
            未打开会话：浏览的是服务默认工作区。打开会话后将切换到该会话的 workdir。
          </p>
        )}
        {loading && (
          <p className="flex items-center gap-2 px-2 py-3 text-[12px] text-slate-500">
            <SpinnerIcon width={13} height={13} />
            加载中…
          </p>
        )}
        {error && (
          <div className="mx-1 rounded border border-rose-900/50 bg-rose-950/30 px-2 py-1.5 text-[11.5px] text-rose-300">
            {error}
          </div>
        )}
        {!loading && !error && path !== '.' && (
          <button
            onClick={() => {
              const parts = path.split('/').filter(Boolean)
              parts.pop()
              void load(parts.length ? parts.join('/') : '.')
            }}
            className="mb-1 flex w-full items-center gap-1.5 rounded px-2 py-1 text-[12px] text-slate-500 hover:bg-ink-800"
          >
            <span className="text-slate-600">..</span>
            上一级
          </button>
        )}
        {!loading && !error && entries.length === 0 && (
          <p className="px-2 py-3 text-center text-[12px] text-slate-600">空目录</p>
        )}
        {entries.map((e) => {
          const isDir = e.type === 'dir'
          const isViewing = viewingPath === e.path
          return (
            <button
              key={e.path}
              onClick={() => (isDir ? void load(e.path) : openFile(e.path, currentId))}
              className={`flex w-full items-center gap-1.5 rounded px-2 py-1 text-left text-[12px] hover:bg-ink-800 ${
                isViewing ? 'bg-brand-600/10 text-brand-300' : 'text-slate-300'
              }`}
              title={e.path}
            >
              {isDir ? (
                <FolderIcon width={13} height={13} className="shrink-0 text-amber-400/80" />
              ) : (
                <DocIcon width={13} height={13} className="shrink-0 text-slate-500" />
              )}
              <span className="min-w-0 flex-1 truncate">{e.name}</span>
              {e.escaped && (
                <span className="rounded border border-rose-800/60 px-1 text-[9.5px] text-rose-400">
                  越界
                </span>
              )}
              {!isDir && e.type === 'file' && (
                <span className="shrink-0 font-mono text-[10px] text-slate-600">
                  {humanSize(e.size)}
                </span>
              )}
            </button>
          )
        })}
        {truncated && (
          <p className="px-2 py-2 text-center text-[11px] text-amber-400/80">
            条目过多，仅显示前 2000 项
          </p>
        )}
      </div>
    </div>
  )
}
