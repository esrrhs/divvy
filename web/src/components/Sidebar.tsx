import { useEffect, useMemo, useState } from 'react'
import { useStore } from '../store'
import type { SessionRow } from '../types'
import { FolderIcon, ListIcon, PlayIcon, PlusIcon, SpinnerIcon } from './icons'
import { FileTree } from './FileTree'

type Filter = 'all' | 'active' | 'done' | 'failed' | 'resumable'

const filters: Array<[Filter, string]> = [
  ['all', '全部'],
  ['active', '进行中'],
  ['resumable', '可续跑'],
  ['done', '完成'],
  ['failed', '失败'],
]

function rowKind(row: SessionRow): Filter {
  if (row.live) {
    const p = row.live.phase
    if (p === 'done') return 'done'
    if (p === 'failed') return 'failed'
    return 'active'
  }
  if (row.root_state === 'COMPLETED') return 'done'
  if (row.root_state === 'FAILED' || row.root_state === 'SKIPPED') return 'failed'
  return 'resumable'
}

function timeAgo(iso: string): string {
  const t = Date.parse(iso)
  if (!Number.isFinite(t)) return ''
  const s = Math.max(0, Math.floor((Date.now() - t) / 1000))
  if (s < 60) return '刚刚'
  const m = Math.floor(s / 60)
  if (m < 60) return `${m} 分钟前`
  const h = Math.floor(m / 60)
  if (h < 24) return `${h} 小时前`
  return new Date(t).toLocaleDateString()
}

function SessionItem({ row }: { row: SessionRow }) {
  const currentId = useStore((s) => s.currentId)
  const openSession = useStore((s) => s.openSession)
  const resumeSession = useStore((s) => s.resumeSession)
  const active = row.id === currentId
  const kind = rowKind(row)
  const progress = row.leaves_all > 0 ? `${row.leaves_done}/${row.leaves_all}` : null
  const [resuming, setResuming] = useState(false)

  const kindLabel: Record<Filter, string> = {
    all: '',
    active: row.live ? phaseLabel(row.live.phase) : '运行中',
    done: '完成',
    failed: '失败',
    resumable: '可续跑',
  }
  const kindColor: Record<Filter, string> = {
    all: '',
    active: 'text-brand-400/90',
    done: 'text-emerald-400/90',
    failed: 'text-rose-400/90',
    resumable: 'text-amber-400/90',
  }

  return (
    <div
      className={`group w-full rounded-md border px-2.5 py-2 transition ${
        active
          ? 'border-brand-600/70 bg-brand-600/10'
          : 'border-transparent hover:border-line hover:bg-ink-800/70'
      }`}
    >
      <button onClick={() => void openSession(row.id)} className="block w-full text-left">
        <div className="line-clamp-2 text-[12.5px] leading-snug text-slate-200">
          {row.goal || '(无标题)'}
        </div>
        <div className="mt-1 flex items-center justify-between text-[11px] text-slate-500">
          <span className="font-mono">{row.id.replace(/^sess_/, '')}</span>
          <span className="flex items-center gap-1.5">
            {progress && <span>{progress} 叶</span>}
            <span className={kindColor[kind]}>{kindLabel[kind]}</span>
            <span>{timeAgo(row.updated_at)}</span>
          </span>
        </div>
      </button>

      {kind === 'resumable' && (
        <button
          onClick={async () => {
            setResuming(true)
            await resumeSession(row.id)
            setResuming(false)
          }}
          className="mt-1.5 flex w-full items-center justify-center gap-1 rounded border border-amber-800/60 bg-amber-950/30 py-1 text-[11px] text-amber-300 transition hover:bg-amber-900/30"
        >
          {resuming ? <SpinnerIcon width={11} height={11} /> : <PlayIcon width={11} height={11} />}
          续跑此会话
        </button>
      )}
    </div>
  )
}

function phaseLabel(p: string): string {
  return (
    {
      planning: '规划中',
      plan_review: '待审批',
      running: '运行中',
      leaf_approval: '待审',
      ask: '问答中',
      paused: '暂停',
      done: '完成',
      failed: '失败',
    } as Record<string, string>
  )[p] ?? p
}

export function Sidebar({ onNewSession }: { onNewSession: () => void }) {
  const [tab, setTab] = useState<'sessions' | 'files'>('sessions')
  const [filter, setFilter] = useState<Filter>('all')
  const sessions = useStore((s) => s.sessions)
  const loading = useStore((s) => s.sessionsLoading)
  const refresh = useStore((s) => s.refreshSessions)

  useEffect(() => {
    void refresh()
  }, [refresh])

  const filtered = useMemo(
    () => (filter === 'all' ? sessions : sessions.filter((r) => rowKind(r) === filter)),
    [sessions, filter],
  )

  return (
    <aside className="flex h-full w-[264px] shrink-0 flex-col border-r border-line bg-ink-850">
      <div className="flex items-center gap-2 px-3 pb-2 pt-3">
        <button onClick={onNewSession} className="btn-primary flex-1 py-1.5 text-[12.5px]" title="新建会话">
          <PlusIcon width={14} height={14} />
          新建会话
        </button>
        <button onClick={() => void refresh()} className="btn-ghost px-2 py-1.5" title="刷新列表">
          {loading ? <SpinnerIcon width={14} height={14} /> : <ListIcon width={14} height={14} />}
        </button>
      </div>

      <div className="flex gap-1 px-3 pb-2">
        {(
          [
            ['sessions', ListIcon, '会话'],
            ['files', FolderIcon, '文件'],
          ] as const
        ).map(([key, Icon, label]) => (
          <button
            key={key}
            onClick={() => setTab(key)}
            className={`flex flex-1 items-center justify-center gap-1.5 rounded-md py-1 text-[12px] transition ${
              tab === key
                ? 'bg-ink-700 text-slate-200'
                : 'text-slate-500 hover:bg-ink-800 hover:text-slate-400'
            }`}
          >
            <Icon width={13} height={13} />
            {label}
          </button>
        ))}
      </div>

      {tab === 'sessions' ? (
        <>
          <div className="flex flex-wrap gap-1 px-3 pb-2">
            {filters.map(([key, label]) => (
              <button
                key={key}
                onClick={() => setFilter(key)}
                className={`rounded-full border px-2 py-0.5 text-[10.5px] transition ${
                  filter === key
                    ? 'border-brand-600/70 bg-brand-600/15 text-brand-300'
                    : 'border-line text-slate-500 hover:text-slate-300'
                }`}
              >
                {label}
              </button>
            ))}
          </div>
          <div className="flex-1 space-y-1 overflow-y-auto px-2 pb-3">
            {sessions.length === 0 && !loading && (
              <p className="px-2 py-6 text-center text-[12px] leading-relaxed text-slate-600">
                还没有会话。
                <br />
                点击「新建会话」开始。
              </p>
            )}
            {sessions.length > 0 && filtered.length === 0 && (
              <p className="px-2 py-6 text-center text-[12px] text-slate-600">该筛选下没有会话。</p>
            )}
            {filtered.map((row) => (
              <SessionItem key={row.id} row={row} />
            ))}
          </div>
        </>
      ) : (
        <FileTree />
      )}
    </aside>
  )
}
