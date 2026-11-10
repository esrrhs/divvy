import { useEffect, useState } from 'react'
import { api } from '../api/client'
import type { FsEntry } from '../types'
import { FolderIcon, SpinnerIcon, XIcon } from './icons'

export function DirPicker({
  initial,
  onSelect,
  onClose,
}: {
  initial: string
  onSelect: (path: string) => void
  onClose: () => void
}) {
  const [path, setPath] = useState(initial)
  const [parent, setParent] = useState('')
  const [entries, setEntries] = useState<FsEntry[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)

  async function load(p: string) {
    setLoading(true)
    setError(null)
    try {
      const res = await api.browse(p)
      setPath(res.path)
      setParent(res.parent)
      setEntries(res.entries)
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => {
    void load(initial)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/55 p-6 backdrop-blur-[1px]">
      <div className="flex max-h-[80vh] w-full max-w-[560px] flex-col overflow-hidden rounded-xl border border-line bg-ink-850 shadow-2xl">
        <div className="flex items-center gap-2 border-b border-line px-4 py-2.5">
          <FolderIcon width={14} height={14} className="text-slate-400" />
          <h3 className="text-[13px] font-semibold text-slate-200">选择项目目录</h3>
          <button onClick={onClose} className="ml-auto text-slate-500 hover:text-slate-300" aria-label="关闭">
            <XIcon width={14} height={14} />
          </button>
        </div>
        <div className="border-b border-line px-4 py-2 font-mono text-[11.5px] text-slate-400">
          {path}
        </div>
        <div className="min-h-0 flex-1 overflow-y-auto px-2 py-2">
          {loading ? (
            <p className="flex items-center gap-2 px-2 py-3 text-[12px] text-slate-500">
              <SpinnerIcon width={12} height={12} />
              读取目录…
            </p>
          ) : error ? (
            <p className="mx-2 rounded border border-rose-900/50 bg-rose-950/30 px-2 py-1.5 text-[11.5px] text-rose-300">
              {error}
            </p>
          ) : (
            <ul>
              {parent ? (
                <li>
                  <button
                    className="flex w-full items-center gap-2 rounded px-2 py-1.5 text-left text-[12.5px] text-slate-300 hover:bg-ink-800"
                    onClick={() => void load(parent)}
                  >
                    <FolderIcon width={13} height={13} className="text-slate-500" />
                    ..
                  </button>
                </li>
              ) : null}
              {entries.filter((e) => e.type === 'dir').map((e) => (
                <li key={e.path}>
                  <button
                    className="flex w-full items-center gap-2 rounded px-2 py-1.5 text-left text-[12.5px] text-slate-300 hover:bg-ink-800"
                    onClick={() => void load(e.path)}
                  >
                    <FolderIcon width={13} height={13} className="text-slate-500" />
                    {e.name}
                  </button>
                </li>
              ))}
              {entries.filter((e) => e.type === 'dir').length === 0 && (
                <li className="px-2 py-3 text-[12px] text-slate-600">此目录下没有子文件夹。</li>
              )}
            </ul>
          )}
        </div>
        <div className="flex justify-end gap-2 border-t border-line px-4 py-2.5">
          <button className="btn-ghost" onClick={onClose}>
            取消
          </button>
          <button className="btn-primary" disabled={!path} onClick={() => onSelect(path)}>
            选择此目录
          </button>
        </div>
      </div>
    </div>
  )
}
