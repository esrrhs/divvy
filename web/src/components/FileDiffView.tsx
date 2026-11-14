import { useEffect, useMemo, useState } from 'react'
import type { ApplyHunkRequest, FileChange, TurnChange } from '../types'
import { appliedStatusLabel } from '../lib/conversation'
import { computeDiff, toHunks, type DiffLine } from '../lib/diff'
import { LangFromPath, tokenizeLines, type Token, type TokenKind } from '../lib/highlight'

// Only the kinds that carry meaning get a colour. Plain text inherits the
// row's colour, so an added line still reads as added (emerald) and a removed
// one as removed (rose) — the syntax colours sit on top of that, the way a
// code review tool is supposed to look.
const TOKEN_CLASS: Record<TokenKind, string> = {
  plain: '',
  keyword: 'text-sky-300',
  type: 'text-teal-300',
  string: 'text-emerald-300',
  number: 'text-amber-300',
  comment: 'text-slate-500 italic',
  func: 'text-brand-300',
}

const statusMeta: Record<FileChange['status'], { label: string; cls: string }> = {
  added: { label: '新增', cls: 'border-emerald-700/60 bg-emerald-900/20 text-emerald-300' },
  modified: { label: '修改', cls: 'border-amber-700/60 bg-amber-900/20 text-amber-300' },
  deleted: { label: '删除', cls: 'border-rose-800/60 bg-rose-900/20 text-rose-300' },
}

export type HunkReviewMode = 'readonly' | 'apply' | 'select'

export interface FileDiffListProps {
  changes: FileChange[]
  /** When omitted, each change's own editable flag is used. */
  editable?: boolean
  reason?: string
  mode?: HunkReviewMode
  onApply?: (payload: ApplyHunkRequest) => Promise<unknown> | void
  onDraftChange?: (drafts: Record<string, ApplyHunkRequest>) => void
  busyPath?: string
}

function asTurn(change: FileChange): TurnChange {
  return change as TurnChange
}

function hunkLines(change: FileChange): { hunks: { index: number; lines: DiffLine[] }[]; truncated: boolean } {
  const tc = asTurn(change)
  if (tc.hunks && tc.hunks.length > 0) {
    return {
      hunks: tc.hunks.map((h) => ({
        index: typeof h.index === 'number' ? h.index : 0,
        lines: (h.lines ?? []).map((l) => ({
          type: l.type === 'add' ? 'add' : l.type === 'remove' ? 'remove' : 'context',
          oldNo: l.old_no ?? null,
          newNo: l.new_no ?? null,
          text: l.text,
        })),
      })),
      truncated: !!change.truncated,
    }
  }
  if (change.binary) return { hunks: [], truncated: false }
  const { lines, truncated } = computeDiff(change.old_content ?? '', change.new_content ?? '')
  return { hunks: toHunks(lines).map((h, i) => ({ index: i, lines: h.lines })), truncated }
}

// changeGroups assigns every line of the diff to a change group: a maximal run
// of consecutive changed lines. A modification shows up as a remove line plus
// an add line, and that pair is ONE change — half of it would delete the line.
//
// The groups are what the review decides on. Per-hunk decisions are too
// coarse: two changes a few lines apart share a hunk (the context around them
// overlaps), so rejecting one of them rejected both.
function changeGroups(hunks: { lines: DiffLine[] }[]): number[] {
	// The group of every line, in the order the hunks show them. A modification
	// is a remove line plus an add line, and the two are PAIRED positionally:
	// two modifications next to each other are two groups, so one can be
	// dropped while the other is kept. The server pairs them the same way
	// (changeGroups in pkg/server/hunks.go) — the two must agree, or the
	// decisions land on the wrong lines.
	const out: number[] = []
	let gi = 0
	let run: { removes: number[]; adds: number[] } = { removes: [], adds: [] }
	const flush = () => {
		if (!run.removes.length && !run.adds.length) return
		const pairs = Math.min(run.removes.length, run.adds.length)
		for (let k = 0; k < pairs; k++) {
			out[run.removes[k]] = gi + k
			out[run.adds[k]] = gi + k
		}
		gi += pairs
		for (let k = pairs; k < run.removes.length; k++) {
			out[run.removes[k]] = gi++
		}
		for (let k = pairs; k < run.adds.length; k++) {
			out[run.adds[k]] = gi++
		}
		run = { removes: [], adds: [] }
	}
	let index = 0
	for (const h of hunks) {
		for (const l of h.lines) {
			if (l.type === 'context') {
				flush()
				out[index] = -1
				index++
				continue
			}
			if (l.type === 'remove') run.removes.push(index)
			else run.adds.push(index)
			index++
		}
	}
	flush()
	return out
}

function DiffRow({
	l,
	tokens,
	group,
	rejected,
	onToggle,
}: {
	l: DiffLine
	tokens?: Token[]
	group: number
	rejected: boolean
	onToggle?: (group: number) => void
}) {
  const cls =
    l.type === 'add'
      ? 'bg-emerald-950/40 text-emerald-200'
      : l.type === 'remove'
        ? 'bg-rose-950/40 text-rose-200'
        : 'text-slate-400'
  const sign = l.type === 'add' ? '+' : l.type === 'remove' ? '-' : ' '
  // A rejected change is shown as reverted: the lines it would have written
  // are dimmed, and the line it would have removed stays as it is.
  const rowCls = rejected ? 'opacity-45' : ''
  return (
    <div
      onClick={onToggle ? () => onToggle(group) : undefined}
      title={onToggle ? '点击这一处改动：保留或撤销（⌘/Ctrl+Z 撤销的是 AI 改写，不是这里）' : undefined}
      className={`flex font-mono text-[11.5px] leading-[1.55] ${cls} ${rowCls} ${
        onToggle ? 'cursor-pointer hover:bg-ink-900/60' : ''
      }`}
    >
      <span className="w-10 shrink-0 select-none border-r border-ink-700/60 pr-2 text-right text-slate-600">
        {l.oldNo ?? ''}
      </span>
      <span className="w-10 shrink-0 select-none border-r border-ink-700/60 pr-2 text-right text-slate-600">
        {l.newNo ?? ''}
      </span>
      <span className="w-4 shrink-0 select-none text-center">{sign}</span>
      <span className="whitespace-pre-wrap break-all">
        {tokens && tokens.length ? (
          tokens.map((t, i) => (
            <span key={i} className={TOKEN_CLASS[t.kind]}>
              {t.text}
            </span>
          ))
        ) : (
          l.text
        )}
      </span>
    </div>
  )
}

function FileDiff({
  change,
  editable,
  mode,
  onApply,
  onLocalChange,
  busy,
}: {
  change: FileChange
  editable: boolean
  mode: HunkReviewMode
  onApply?: (payload: ApplyHunkRequest) => Promise<unknown> | void
  onLocalChange?: (path: string, payload: ApplyHunkRequest) => void
  busy?: boolean
}) {
  const [open, setOpen] = useState(change.status !== 'modified')
  const meta = statusMeta[change.status]
  const tc = asTurn(change)
  const { hunks, truncated } = useMemo(() => hunkLines(change), [change])
  // The decisions are per change group, not per hunk: two changes a few lines
  // apart share a hunk, and the user has to be able to keep one and drop the
  // other.
  const groupOf = useMemo(() => changeGroups(hunks), [hunks])
  const hunksLayout = useMemo(() => {
		const out: { groups: number[]; offset: number }[] = []
		let at = 0
		for (const h of hunks) {
			const set = new Set<number>()
			for (let k = 0; k < h.lines.length; k++) {
				const g = groupOf[at + k]
				if (g >= 0) set.add(g)
			}
			out.push({ groups: [...set], offset: at })
			at += h.lines.length
		}
		return out
	}, [hunks, groupOf])
  const groupCount = useMemo(() => {
		let max = -1
		for (const g of groupOf) if (g > max) max = g
		return max + 1
	}, [groupOf])
	const [rejectedGroups, setRejectedGroups] = useState<Set<number>>(new Set())

  // Syntax colours for the reviewed code. The tokens come from the WHOLE file
  // (not from the diff line) so a comment or a raw string that spans lines
  // keeps its state: its tail must not be coloured as code.
  const lang = LangFromPath(change.path)
  const oldTokens = useMemo(
    () => tokenizeLines(change.old_content ?? '', lang),
    [change.old_content, lang],
  )
  const newTokens = useMemo(
    () => tokenizeLines(change.new_content ?? '', lang),
    [change.new_content, lang],
  )
  const tokensFor = (l: DiffLine): Token[] | undefined => {
    if (l.type === 'remove') return l.oldNo ? oldTokens[l.oldNo - 1] : undefined
    if (l.newNo) return newTokens[l.newNo - 1]
    if (l.oldNo) return oldTokens[l.oldNo - 1]
    return undefined
  }

  // The stored decisions are per hunk; the group model derives them from it.
  useEffect(() => {
    const stored = asTurn(change).hunk_decisions
    const next = new Set<number>()
    if (stored) {
      hunksLayout.forEach(({ groups }, hi) => {
        const v = (stored[hi] ?? '').toLowerCase()
        const wasAccepted = v === 'accept' || v === 'accepted' || v === 'true'
        if (!wasAccepted) groups.forEach((g: number) => next.add(g))
      })
    }
    setRejectedGroups(next)
  }, [change, hunksLayout])

  const interactive = editable && mode !== 'readonly' && tc.applied_status !== 'rolled_back'
  const applied = appliedStatusLabel(tc.applied_status)

  function payloadFor(next: Set<number>, all?: 'accept' | 'reject'): ApplyHunkRequest {
    if (change.binary || all) {
      return { path: change.path, all: all ?? 'accept' }
    }
    const decisions: boolean[] = []
    for (let g = 0; g < groupCount; g++) decisions.push(!next.has(g))
    return { path: change.path, group_decisions: decisions }
  }

  async function chooseGroup(group: number, accept: boolean) {
    const next = new Set(rejectedGroups)
    if (accept) next.delete(group)
    else next.add(group)
    setRejectedGroups(next)
    const payload = payloadFor(next)
    onLocalChange?.(change.path, payload)
    if (mode === 'apply') await onApply?.(payload)
  }

  async function choose(index: number, accept: boolean) {
    const next = new Set(rejectedGroups)
    for (const g of hunksLayout[index]?.groups ?? []) {
      if (accept) next.delete(g)
      else next.add(g)
    }
    setRejectedGroups(next)
    const payload = payloadFor(next)
    onLocalChange?.(change.path, payload)
    if (mode === 'apply') await onApply?.(payload)
  }

  async function chooseAll(all: 'accept' | 'reject') {
    const next = new Set<number>()
    if (all === 'reject') {
      for (let g = 0; g < groupCount; g++) next.add(g)
    }
    setRejectedGroups(next)
    const payload = payloadFor(next, all)
    onLocalChange?.(change.path, payload)
    if (mode === 'apply') await onApply?.(payload)
  }

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
        {applied && (
          <span className="rounded border border-sky-800/60 px-1.5 py-px text-[10.5px] text-sky-300">{applied}</span>
        )}
        <span className="ml-auto text-[10.5px] text-slate-600">
          {change.truncated ? '内容已截断显示' : change.binary ? '二进制' : hunks.length ? `${hunks.length} 个改动块` : ''}
        </span>
      </button>

      {open && (
        <div className="bg-ink-950/70 py-1">
          {interactive && (
            <div className="flex flex-wrap items-center gap-2 border-b border-line/60 px-3 py-1.5">
              <button
                type="button"
                className="btn px-2 py-0.5 text-[11px]"
                disabled={busy}
                onClick={(e) => {
                  e.stopPropagation()
                  void chooseAll('accept')
                }}
              >
                {change.binary ? '整份接受' : '全部接受'}
              </button>
              <button
                type="button"
                className="btn px-2 py-0.5 text-[11px]"
                disabled={busy}
                onClick={(e) => {
                  e.stopPropagation()
                  void chooseAll('reject')
                }}
              >
                {change.binary ? '整份拒绝' : '全部拒绝'}
              </button>
              {busy && <span className="text-[11px] text-slate-500">正在写回工作区…</span>}
              {mode === 'select' && (
                <span className="text-[11px] text-slate-500">未点的块默认接受；批准时才会合并到工作区</span>
              )}
              {mode === 'apply' && (
                <span className="text-[11px] text-slate-500">决定会立即写回当前工作区，只影响这一轮的快照对账</span>
              )}
            </div>
          )}
          {change.binary ? (
            <p className="px-3 py-3 text-[12px] text-slate-500">
              二进制文件，无法展示行级 diff（
              {change.status === 'added' ? '新增' : change.status === 'deleted' ? '删除' : '修改'}
              ）。{interactive ? '只能整份接受或拒绝。' : ''}
            </p>
          ) : hunks.length === 0 ? (
            <p className="px-3 py-3 text-[12px] text-slate-500">内容无差异（可能仅权限变化）。</p>
          ) : (
            <div className="max-h-[320px] overflow-auto">
              {truncated && (
                <p className="border-b border-line bg-amber-950/30 px-3 py-1 text-[11px] text-amber-300/90">
                  文件较大，diff 仅覆盖前 {4000} 行
                </p>
              )}
              {hunks.map((h, i) => {
                const hunkGroups = hunksLayout[i]?.groups ?? []
                const lineAt = hunksLayout[i]?.offset ?? 0
                const accepted = hunkGroups.every((g) => !rejectedGroups.has(g))
                return (
                  <div key={h.index ?? i} className={i > 0 ? 'border-t border-line/60' : ''}>
                    {interactive && !change.binary && (
                      <div className="flex items-center gap-2 px-3 py-1 text-[11px] text-slate-500">
                        <span>第 {i + 1} 块</span>
                        <button
                          type="button"
                          disabled={busy}
                          className={`rounded border px-1.5 py-px ${
                            accepted
                              ? 'border-emerald-700/70 bg-emerald-900/30 text-emerald-300'
                              : 'border-line text-slate-500 hover:text-slate-300'
                          }`}
                          onClick={() => void choose(i, true)}
                        >
                          接受
                        </button>
                        <button
                          type="button"
                          disabled={busy}
                          className={`rounded border px-1.5 py-px ${
                            !accepted
                              ? 'border-rose-800/70 bg-rose-950/40 text-rose-300'
                              : 'border-line text-slate-500 hover:text-slate-300'
                          }`}
                          onClick={() => void choose(i, false)}
                        >
                          拒绝
                        </button>
                      </div>
                    )}
                    {h.lines.map((l, j) => {
                      const g = groupOf[lineAt + j] ?? -1
                      const rejected = g >= 0 && rejectedGroups.has(g)
                      return (
                        <DiffRow
                          key={j}
                          l={l}
                          tokens={tokensFor(l)}
                          group={g}
                          rejected={rejected}
                          onToggle={interactive && g >= 0 ? (grp) => void chooseGroup(grp, rejected) : undefined}
                        />
                      )
                    })}
                  </div>
                )
              })}
            </div>
          )}
        </div>
      )}
    </div>
  )
}

export function FileDiffList({
  changes,
  editable,
  reason,
  mode = 'readonly',
  onApply,
  onDraftChange,
  busyPath,
}: FileDiffListProps) {
  const [drafts, setDrafts] = useState<Record<string, ApplyHunkRequest>>({})

  useEffect(() => {
    setDrafts({})
  }, [changes.map((c) => c.path).join('\0')])

  if (changes.length === 0) {
    return <p className="text-[12px] text-slate-500">本次没有文件变更。</p>
  }
  const groups = {
    modified: changes.filter((c) => c.status === 'modified'),
    added: changes.filter((c) => c.status === 'added'),
    deleted: changes.filter((c) => c.status === 'deleted'),
  }
  const canEdit = editable === true || (editable === undefined && changes.some((c) => asTurn(c).editable))
  return (
    <div className="space-y-2">
      {reason && !canEdit && mode !== 'select' && (
        <p className="text-[11.5px] leading-snug text-slate-500">{reason}</p>
      )}
      {mode === 'select' && (
        <p className="text-[11.5px] leading-snug text-slate-500">
          按块选择后点「按已选块批准合并」。未选择的块默认接受。拒绝仍会丢掉这次叶子的全部改动并要求重做，不会绕过审批。
        </p>
      )}
      {mode === 'apply' && canEdit && (
        <p className="text-[11.5px] leading-snug text-slate-500">
          可以按块接受或拒绝，决定会写回当前工作区。较早轮次只能只读回看，以免误改现在的文件。
        </p>
      )}
      {[...groups.modified, ...groups.added, ...groups.deleted].map((c) => (
        <FileDiff
          key={c.path}
          change={c}
          editable={editable ?? !!asTurn(c).editable}
          mode={mode}
          busy={busyPath === c.path}
          onApply={onApply}
          onLocalChange={(path, payload) => {
            const next = { ...drafts, [path]: payload }
            setDrafts(next)
            onDraftChange?.(next)
          }}
        />
      ))}
    </div>
  )
}
