import { useEffect, useMemo, useState } from 'react'
import { conversationRowKind, shortWorkdir } from '../lib/conversation'
import { useStore } from '../store'
import type { Conversation } from '../types'
import { FolderIcon, ListIcon, PlayIcon, PlusIcon, SearchIcon, SpinnerIcon, TrashIcon } from './icons'
import { FileTree } from './FileTree'
import { ProjectSearch } from './ProjectSearch'

type Filter = 'all' | 'active' | 'paused' | 'done' | 'failed' | 'idle'

const filters: Array<[Filter, string]> = [
  ['all', '全部'],
  ['active', '进行中'],
  ['paused', '已暂停'],
  ['idle', '可继续'],
  ['done', '完成'],
  ['failed', '失败'],
]

function rowKind(row: Conversation): Filter {
  return conversationRowKind(row.run_phase) as Filter
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

function ConversationItem({ row }: { row: Conversation }) {
  const currentId = useStore((s) => s.currentConvId)
  const openConversation = useStore((s) => s.openConversation)
  const resumeConversation = useStore((s) => s.resumeConversation)
  const deleteConversation = useStore((s) => s.deleteConversation)
  const active = row.id === currentId
  const kind = rowKind(row)
  const [resuming, setResuming] = useState(false)
  const [deleting, setDeleting] = useState(false)

  const kindLabel: Record<Filter, string> = {
    all: '',
    active: '进行中',
    done: '完成',
    failed: '失败',
    paused: '已暂停',
    idle: '可继续',
  }
  const kindColor: Record<Filter, string> = {
    all: '',
    active: 'text-brand-400/90',
    done: 'text-emerald-400/90',
    failed: 'text-rose-400/90',
    paused: 'text-amber-400/90',
    idle: 'text-slate-400',
  }

  const onDelete = async () => {
    const title = (row.title || row.workdir || '(无标题)').slice(0, 60)
    const ok = window.confirm(
      `确定删除对话「${title}」吗？\n\n对话记录会被删除，项目目录里的文件不会改动。`,
    )
    if (!ok) return
    setDeleting(true)
    await deleteConversation(row.id)
    setDeleting(false)
  }

  return (
    <div
      className={`group relative w-full rounded-md border px-2.5 py-2 transition ${
        active
          ? 'border-brand-600/70 bg-brand-600/10'
          : 'border-transparent hover:border-line hover:bg-ink-800/70'
      }`}
    >
      {kind !== 'active' && (
        <button
          onClick={() => void onDelete()}
          disabled={deleting}
          title="删除对话"
          className="absolute right-1.5 top-1.5 rounded p-0.5 text-slate-600 opacity-0 transition hover:bg-ink-700 hover:text-rose-400 focus:opacity-100 group-hover:opacity-100 disabled:opacity-60"
        >
          {deleting ? <SpinnerIcon width={12} height={12} /> : <TrashIcon width={12} height={12} />}
        </button>
      )}
      <button onClick={() => void openConversation(row.id)} className="block w-full text-left">
        <div className="line-clamp-2 pr-5 text-[12.5px] leading-snug text-slate-200">
          {row.title || shortWorkdir(row.workdir) || '(无标题)'}
        </div>
        <div className="mt-1 flex items-center justify-between text-[11px] text-slate-500">
          <span className="truncate font-mono" title={row.workdir}>
            {shortWorkdir(row.workdir)}
          </span>
          <span className="flex shrink-0 items-center gap-1.5">
            <span className={kindColor[kind]}>{kindLabel[kind]}</span>
            <span>{timeAgo(row.updated_at)}</span>
          </span>
        </div>
      </button>

      {kind === 'paused' && (
        <button
          onClick={async () => {
            setResuming(true)
            if (row.id !== currentId) await openConversation(row.id)
            await resumeConversation()
            setResuming(false)
          }}
          className="mt-1.5 flex w-full items-center justify-center gap-1 rounded border border-amber-800/60 bg-amber-950/30 py-1 text-[11px] text-amber-300 transition hover:bg-amber-900/30"
        >
          {resuming ? <SpinnerIcon width={11} height={11} /> : <PlayIcon width={11} height={11} />}
          续跑此对话
        </button>
      )}
    </div>
  )
}

export function Sidebar({ onNewSession }: { onNewSession: () => void }) {
  const [tab, setTab] = useState<'sessions' | 'files' | 'search'>('sessions')
  const [filter, setFilter] = useState<Filter>('all')
  const conversations = useStore((s) => s.conversations)
  const loading = useStore((s) => s.conversationsLoading)
  const refresh = useStore((s) => s.refreshConversations)

  useEffect(() => {
    void refresh()
  }, [refresh])

  const filtered = useMemo(
    () => (filter === 'all' ? conversations : conversations.filter((r) => rowKind(r) === filter)),
    [conversations, filter],
  )

  return (
    <aside className="flex h-full w-[264px] shrink-0 flex-col border-r border-line bg-ink-850">
      <div className="flex items-center gap-2 px-3 pb-2 pt-3">
        <button onClick={onNewSession} className="btn-primary flex-1 py-1.5 text-[12.5px]" title="打开项目">
          <PlusIcon width={14} height={14} />
          打开项目
        </button>
        <button onClick={() => void refresh()} className="btn-ghost px-2 py-1.5" title="刷新列表">
          {loading ? <SpinnerIcon width={14} height={14} /> : <ListIcon width={14} height={14} />}
        </button>
      </div>

      <div className="flex gap-1 px-3 pb-2">
        {(
          [
            ['sessions', ListIcon, '对话'],
            ['files', FolderIcon, '文件'],
            ['search', SearchIcon, '搜索'],
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
            {conversations.length === 0 && !loading && (
              <p className="px-2 py-6 text-center text-[12px] leading-relaxed text-slate-600">
                还没有对话。
                <br />
                点击「打开项目」选择一个已有目录。
              </p>
            )}
            {conversations.length > 0 && filtered.length === 0 && (
              <p className="px-2 py-6 text-center text-[12px] text-slate-600">该筛选下没有对话。</p>
            )}
            {filtered.map((row) => (
              <ConversationItem key={row.id} row={row} />
            ))}
          </div>
        </>
      ) : tab === 'files' ? (
        <FileTree />
      ) : (
        <ProjectSearch />
      )}
    </aside>
  )
}
