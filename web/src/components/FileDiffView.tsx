import { useMemo, useState } from 'react'
import type { FileChange } from '../types'
import { computeDiff, toHunks, type DiffLine } from '../lib/diff'

const statusMeta: Record<FileChange['status'], { label: string; cls: string }> = {
  added: { label: '新增', cls: 'border-emerald-700/60 bg-emerald-900/20 text-emerald-300' },
  modified: { label: '修改', cls: 'border-amber-700/60 bg-amber-900/20 text-amber-300' },
  deleted: { label: '删除', cls: 'border-rose-800/60 bg-rose-900/20 text-rose-300' },
}

function DiffRow({ l }: { l: DiffLine }) {
  const cls =
    l.type === 'add'
      ? 'bg-emerald-950/40 text-emerald-200'
      : l.type === 'remove'
        ? 'bg-rose-950/40 text-rose-200'
        : 'text-slate-400'
  const sign = l.type === 'add' ? '+' : l.type === 'remove' ? '-' : ' '
  return (
    <div className={`flex font-mono text-[11.5px] leading-[1.55] ${cls}`}>
      <span className="w-10 shrink-0 select-none border-r border-ink-700/60 pr-2 text-right text-slate-600">
        {l.oldNo ?? ''}
      </span>
      <span className="w-10 shrink-0 select-none border-r border-ink-700/60 pr-2 text-right text-slate-600">
        {l.newNo ?? ''}
      </span>
      <span className="w-4 shrink-0 select-none text-center">{sign}</span>
      <span className="whitespace-pre-wrap break-all">{l.text}</span>
    </div>
  )
}

function FileDiff({ change }: { change: FileChange }) {
  const [open, setOpen] = useState(change.status !== 'modified')
  const meta = statusMeta[change.status]

  const { hunks, truncated } = useMemo(() => {
    if (change.binary) return { hunks: [], truncated: false }
    const { lines, truncated } = computeDiff(change.old_content ?? '', change.new_content ?? '')
    return { hunks: toHunks(lines), truncated }
  }, [change])

  return (
    <div className="overflow-hidden rounded-md border border-line">
      <button
        type="button"
        onClick={() => setOpen((v) => !v)}
        className="flex w-full items-center gap-2 bg-ink-800/70 px-3 py-1.5 text-left hover:bg-ink-750"
      >
        <svg width="10" height="10" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.5" className={`text-slate-500 transition-transform ${open ? 'rotate-90' : ''}`}>
          <path d="M9 6l6 6-6 6" />
        </svg>
        <span className="font-mono text-[12px] text-slate-200">{change.path}</span>
        <span className={`rounded border px-1.5 py-px text-[10.5px] ${meta.cls}`}>{meta.label}</span>
        {change.conflict && (
          <span className="rounded border border-rose-700 px-1.5 py-px text-[10.5px] text-rose-300">
            合并冲突
          </span>
        )}
        <span className="ml-auto text-[10.5px] text-slate-600">
          {change.truncated ? '内容已截断显示' : change.binary ? '二进制' : ''}
        </span>
      </button>

      {open && (
        <div className="max-h-[320px] overflow-auto bg-ink-950/70 py-1">
          {change.binary ? (
            <p className="px-3 py-3 text-[12px] text-slate-500">
              二进制文件，无法展示行级 diff（
              {change.status === 'added' ? '新增' : change.status === 'deleted' ? '删除' : '修改'}）。
            </p>
          ) : change.status === 'modified' ? (
            hunks.length === 0 ? (
              <p className="px-3 py-3 text-[12px] text-slate-500">内容无差异（可能仅权限变化）。</p>
            ) : (
              <>
                {truncated && (
                  <p className="border-b border-line bg-amber-950/30 px-3 py-1 text-[11px] text-amber-300/90">
                    文件较大，diff 仅覆盖前 {4000} 行
                  </p>
                )}
                {hunks.map((h, i) => (
                  <div key={i} className={i > 0 ? 'border-t border-line/60' : ''}>
                    {h.lines.map((l, j) => (
                      <DiffRow key={j} l={l} />
                    ))}
                  </div>
                ))}
              </>
            )
          ) : (
            <FullContent change={change} />
          )}
        </div>
      )}
    </div>
  )
}

function FullContent({ change }: { change: FileChange }) {
  const content = change.status === 'deleted' ? change.old_content ?? '' : change.new_content ?? ''
  const lines = content.split('\n')
  if (lines[lines.length - 1] === '') lines.pop()
  const add = change.status === 'added'
  return (
    <div className="max-h-[320px] overflow-auto bg-ink-950/70 py-1">
      {lines.map((text, i) => (
        <DiffRow
          key={i}
          l={{
            type: add ? 'add' : 'remove',
            oldNo: add ? null : i + 1,
            newNo: add ? i + 1 : null,
            text,
          }}
        />
      ))}
    </div>
  )
}

export function FileDiffList({ changes }: { changes: FileChange[] }) {
  if (changes.length === 0) {
    return <p className="text-[12px] text-slate-500">本次叶子没有文件变更。</p>
  }
  const groups = {
    modified: changes.filter((c) => c.status === 'modified'),
    added: changes.filter((c) => c.status === 'added'),
    deleted: changes.filter((c) => c.status === 'deleted'),
  }
  return (
    <div className="space-y-2">
      {[...groups.modified, ...groups.added, ...groups.deleted].map((c) => (
        <FileDiff key={c.path} change={c} />
      ))}
    </div>
  )
}
