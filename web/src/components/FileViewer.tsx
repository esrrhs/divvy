import { useEffect, useState } from 'react'
import { ApiError, api } from '../api/client'
import { useStore } from '../store'
import type { FsFileResponse } from '../types'
import { DocIcon, SpinnerIcon, XIcon } from './icons'

function humanSize(n: number): string {
  if (n < 1024) return `${n} B`
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`
  return `${(n / 1024 / 1024).toFixed(2)} MB`
}

// Read-only, sandbox-confined text viewer for workspace files. The server
// already enforces path confinement, a 1 MiB size cap (413) and a binary
// sniff (415); this component renders those outcomes as friendly panels.
export function FileViewer({ sessionId, path }: { sessionId: string | null; path: string }) {
  const closeFile = useStore((s) => s.closeFile)
  const [file, setFile] = useState<FsFileResponse | null>(null)
  const [loading, setLoading] = useState(true)
  const [problem, setProblem] = useState<{ status: number; message: string } | null>(null)

  useEffect(() => {
    let cancelled = false
    setLoading(true)
    setProblem(null)
    setFile(null)
    api
      .fsFile(path, sessionId ?? undefined)
      .then((f) => {
        if (!cancelled) setFile(f)
      })
      .catch((e) => {
        if (cancelled) return
        if (e instanceof ApiError) {
          setProblem({
            status: e.status,
            message:
              e.status === 413
                ? '文件超过 1 MiB 的只读查看上限，请在编辑器或终端中打开。'
                : e.status === 415
                  ? '二进制文件不支持文本预览。'
                  : e.status === 403
                    ? '该路径通过软链接指向工作区之外，已被安全策略阻止。'
                    : e.message,
          })
        } else {
          setProblem({ status: 0, message: String(e) })
        }
      })
      .finally(() => {
        if (!cancelled) setLoading(false)
      })
    return () => {
      cancelled = true
    }
  }, [path, sessionId])

  const lines = file ? file.content.split('\n') : []
  if (lines.length > 1 && lines[lines.length - 1] === '') lines.pop()

  return (
    <div className="flex flex-1 flex-col overflow-hidden">
      <div className="flex shrink-0 items-center gap-2 border-b border-line bg-ink-850 px-4 py-2">
        <DocIcon width={14} height={14} className="text-slate-500" />
        <span className="truncate font-mono text-[12px] text-slate-200" title={path}>
          {path}
        </span>
        {file && (
          <span className="shrink-0 font-mono text-[11px] text-slate-500">
            {humanSize(file.size)} · {lines.length} 行 · 只读
          </span>
        )}
        <button
          onClick={closeFile}
          className="ml-auto flex shrink-0 items-center gap-1 rounded border border-line px-2 py-0.5 text-[11.5px] text-slate-400 hover:bg-ink-800 hover:text-slate-200"
        >
          <XIcon width={12} height={12} />
          关闭
        </button>
      </div>

      <div className="min-h-0 flex-1 overflow-auto bg-ink-950/60">
        {loading && (
          <p className="flex items-center gap-2 px-5 py-6 text-[12.5px] text-slate-500">
            <SpinnerIcon width={14} height={14} />
            正在读取 {path}…
          </p>
        )}
        {problem && (
          <div className="mx-5 my-6 max-w-[560px] rounded-lg border border-rose-900/50 bg-rose-950/25 px-4 py-3">
            <p className="text-[13px] font-medium text-rose-300">无法预览文件</p>
            <p className="mt-1 text-[12px] leading-relaxed text-rose-200/80">{problem.message}</p>
            <p className="mt-2 font-mono text-[11px] text-slate-500">
              {path}
              {problem.status > 0 ? ` · HTTP ${problem.status}` : ''}
            </p>
          </div>
        )}
        {file && (
          <table className="w-full border-collapse font-mono text-[12px] leading-[1.6]">
            <tbody>
              {lines.map((text, i) => (
                <tr key={i} className="hover:bg-ink-900/60">
                  <td className="select-none border-r border-ink-800 px-3 text-right text-[10.5px] text-slate-700">
                    {i + 1}
                  </td>
                  <td className="whitespace-pre-wrap break-all px-3 text-slate-300">{text || ' '}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>
    </div>
  )
}
