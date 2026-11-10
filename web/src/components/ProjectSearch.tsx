import { useEffect, useRef, useState } from 'react'
import { ApiError, api } from '../api/client'
import { citationFromSearchHit, draftLocateNotice, hitLabel, projectSearchCaption } from '../lib/search'
import {
  applySearchResult,
  beginSearch,
  canActOnSearchHits,
  emptySearchView,
  invalidateSearch,
  sameSearchScope,
  type SearchScope,
} from '../lib/projectSearchSession'
import { useStore } from '../store'
import type { ProjectSearchHit } from '../types'
import { SearchIcon, SpinnerIcon } from './icons'

export function ProjectSearch() {
  const currentConvId = useStore((s) => s.currentConvId)
  const currentId = useStore((s) => s.currentId)
  const openFile = useStore((s) => s.openFile)
  const attachSelection = useStore((s) => s.attachSelection)
  const fileDirty = useStore((s) => s.fileDirty)
  const fileView = useStore((s) => s.fileView)

  const [q, setQ] = useState('')
  const [caseSensitive, setCaseSensitive] = useState(false)
  const [regex, setRegex] = useState(false)
  const [glob, setGlob] = useState('')
  const [note, setNote] = useState<string | null>(null)
  const scope: SearchScope = { conversationId: currentConvId, sessionId: currentId }
  const [view, setView] = useState(() => emptySearchView(scope, 0))
  const gen = useRef(0)
  const mountedRef = useRef(true)
  const scopeRef = useRef(scope)
  scopeRef.current = scope

  async function run(query: string, token: number, requestScope: SearchScope) {
    const text = query.trim()
    if (!(requestScope.conversationId || requestScope.sessionId) || !text) return
    setView((v) => beginSearch(v, token, requestScope))
    try {
      const got = await api.fsSearch(text, {
        conversation: requestScope.conversationId ?? undefined,
        session: requestScope.sessionId ?? undefined,
        caseSensitive,
        regex,
        glob: glob.trim() || undefined,
        limit: 80,
      })
      setView((v) =>
        applySearchResult(v, token, requestScope, {
          hits: got.hits ?? [],
          truncated: !!got.truncated,
          scanned: got.scanned ?? 0,
        }),
      )
    } catch (e) {
      const message = e instanceof ApiError ? e.message : String(e)
      setView((v) => applySearchResult(v, token, requestScope, { error: message }))
    }
  }

  useEffect(() => {
    const requestScope: SearchScope = { conversationId: currentConvId, sessionId: currentId }
    const token = ++gen.current
    setView((v) => invalidateSearch(v, requestScope, token))
    setNote(null)
    const text = q.trim()
    if (!(currentConvId || currentId)) {
      setView((v) =>
        v.generation === token
          ? { ...v, error: '请先打开一个项目对话，搜索只在当前项目内进行。' }
          : v,
      )
      return
    }
    if (!text) return
    const t = window.setTimeout(() => void run(q, token, requestScope), 300)
    return () => {
      window.clearTimeout(t)
      gen.current += 1
    }
    // Re-run when options change too.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [q, caseSensitive, regex, glob, currentConvId, currentId])

  useEffect(() => {
    mountedRef.current = true
    return () => {
      mountedRef.current = false
      gen.current += 1
    }
  }, [])


  function openHit(hit: ProjectSearchHit) {
    if (!canActOnSearchHits(view, scope)) return
    const sameOpen = fileView?.path === hit.path
    if (sameOpen && fileDirty) {
      setNote(draftLocateNotice(hit.path, hit.line))
    } else {
      setNote(null)
    }
    // preferDiff: false so a same-file click leaves "本轮差异" and actually reveals the line.
    openFile(hit.path, view.scope.sessionId ?? currentId, { line: hit.line, preferDiff: false })
  }

  async function citeHit(hit: ProjectSearchHit) {
    if (!canActOnSearchHits(view, scope)) return
    const token = view.generation
    const requestScope = view.scope
    const result = await citationFromSearchHit(hit, (path) =>
      api.fsFile(path, requestScope.sessionId ?? undefined, requestScope.conversationId ?? undefined),
    )
    // Bind to the source scope and this instance's lifetime. After unmount
    // (for example switching the sidebar off "搜索") a late fsFile must not
    // attach A's source to whichever conversation is now current.
    if (!mountedRef.current || gen.current !== token || !sameSearchScope(scopeRef.current, requestScope)) {
      return
    }
    if (result.selection) {
      attachSelection(result.selection, { conversationId: requestScope.conversationId ?? undefined })
    }
    setNote(result.notice)
  }

  const { loading, error, hits, truncated, scanned } = view
  const hitsActionable = canActOnSearchHits(view, scope)

  return (
    <div className="flex min-h-0 flex-1 flex-col">
      <form
        className="shrink-0 space-y-2 border-b border-line px-3 pb-2"
        onSubmit={(e) => {
          e.preventDefault()
          const requestScope: SearchScope = { conversationId: currentConvId, sessionId: currentId }
          const token = ++gen.current
          setView((v) => invalidateSearch(v, requestScope, token))
          void run(q, token, requestScope)
        }}
      >
        <label className="block">
          <span className="sr-only">搜索关键字</span>
          <span className="relative block">
            <SearchIcon
              width={13}
              height={13}
              className="pointer-events-none absolute left-2 top-1/2 -translate-y-1/2 text-slate-500"
            />
            <input
              value={q}
              onChange={(e) => setQ(e.target.value)}
              placeholder="搜索项目文件内容"
              className="w-full rounded-md border border-line bg-ink-900 py-1.5 pl-7 pr-2 text-[12px] text-slate-200 outline-none placeholder:text-slate-600 focus:border-brand-600/70"
            />
          </span>
        </label>
        <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-[10.5px] text-slate-400">
          <label className="flex items-center gap-1">
            <input type="checkbox" checked={caseSensitive} onChange={(e) => setCaseSensitive(e.target.checked)} />
            区分大小写
          </label>
          <label className="flex items-center gap-1">
            <input type="checkbox" checked={regex} onChange={(e) => setRegex(e.target.checked)} />
            正则
          </label>
          <input
            value={glob}
            onChange={(e) => setGlob(e.target.value)}
            placeholder="文件过滤，如 *.go"
            className="min-w-0 flex-1 rounded border border-line bg-ink-900 px-1.5 py-0.5 text-[10.5px] text-slate-300 outline-none placeholder:text-slate-600"
          />
        </div>
        <p className="text-[10.5px] leading-relaxed text-slate-600">{projectSearchCaption}</p>
      </form>

      {note && <p className="shrink-0 border-b border-line px-3 py-1.5 text-[11px] text-sky-200/90">{note}</p>}
      {error && <p className="shrink-0 border-b border-line px-3 py-1.5 text-[11px] text-rose-300">{error}</p>}

      <div className="min-h-0 flex-1 overflow-y-auto px-1 py-1">
        {loading && (
          <p className="flex items-center gap-1.5 px-2 py-4 text-[12px] text-slate-500">
            <SpinnerIcon width={12} height={12} />
            正在搜索…
          </p>
        )}
        {!loading && hits && hits.length === 0 && (
          <p className="px-2 py-6 text-center text-[12px] text-slate-600">当前项目里没有匹配的内容。</p>
        )}
        {!loading && hits && hits.length > 0 && (
          <ul className="space-y-0.5">
            {hits.map((hit, i) => (
              <li key={`${hit.path}:${hit.line}:${i}`}>
                <div className="group rounded-md px-2 py-1.5 hover:bg-ink-800/80">
                  <button type="button" onClick={() => openHit(hit)} className="block w-full text-left" disabled={!hitsActionable}>
                    <div className="truncate font-mono text-[11.5px] text-slate-200" title={hitLabel(hit)}>
                      {hitLabel(hit)}
                    </div>
                    <div className="mt-0.5 truncate font-mono text-[11px] text-slate-500" title={hit.preview}>
                      {hit.preview}
                    </div>
                  </button>
                  <button
                    type="button"
                    onClick={() => void citeHit(hit)}
                    disabled={!hitsActionable}
                    className="mt-0.5 text-[10.5px] text-slate-500 hover:text-brand-300 disabled:text-slate-700"
                  >
                    引用到对话
                  </button>
                </div>
              </li>
            ))}
          </ul>
        )}
        {!loading && hits && truncated && (
          <p className="px-2 py-2 text-[10.5px] text-amber-200/80">结果已达上限，请缩小关键字或加上文件过滤。</p>
        )}
        {!loading && hits && hits.length > 0 && !truncated && scanned > 0 && (
          <p className="px-2 py-2 text-[10.5px] text-slate-600">已扫描 {scanned} 个文件。</p>
        )}
      </div>
    </div>
  )
}
