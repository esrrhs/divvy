// Line-level diff for leaf review: the backend ships full old/new contents
// (works identically for git and non-git workspaces), so the UI computes an
// LCS-based unified view itself. No diff library dependency.

export type DiffLineType = 'context' | 'add' | 'remove'

export interface DiffLine {
  type: DiffLineType
  /** Line number in the new file (add/context) or null for removals. */
  newNo: number | null
  /** Line number in the old file (remove/context) or null for adds. */
  oldNo: number | null
  text: string
}

export interface DiffHunk {
  lines: DiffLine[]
}

function splitLines(s: string): string[] {
  if (s === '') return []
  // A trailing newline is the normal case for source files; split without
  // producing a phantom empty last line.
  const t = s.endsWith('\n') ? s.slice(0, -1) : s
  return t.split('\n')
}

// lcsTable computes the classic LCS DP table for two line arrays.
function lcsTable(a: string[], b: string[]): number[][] {
  const m = a.length
  const n = b.length
  // Rows as Int32Array keep a 2000×2000 worst case around 16 MB, far
  // cheaper than a generic nested array for big generated files.
  const dp: Int32Array[] = new Array(m + 1)
  for (let i = 0; i <= m; i++) dp[i] = new Int32Array(n + 1)
  for (let i = m - 1; i >= 0; i--) {
    for (let j = n - 1; j >= 0; j--) {
      dp[i][j] = a[i] === b[j] ? dp[i + 1][j + 1] + 1 : Math.max(dp[i + 1][j], dp[i][j + 1])
    }
  }
  return dp as unknown as number[][]
}

const MAX_DIFF_LINES = 4000

export function computeDiff(oldText: string, newText: string): {
  lines: DiffLine[]
  truncated: boolean
} {
  const a = splitLines(oldText ?? '')
  const b = splitLines(newText ?? '')
  if (a.length + b.length > MAX_DIFF_LINES) {
    return { lines: lcsWalk(a, b), truncated: true }
  }
  return { lines: lcsWalk(a, b), truncated: false }
}

function lcsWalk(a: string[], b: string[]): DiffLine[] {
  const dp = lcsTable(a, b)
  const out: DiffLine[] = []
  let i = 0
  let j = 0
  let oldNo = 0
  let newNo = 0
  while (i < a.length && j < b.length) {
    if (a[i] === b[j]) {
      oldNo++
      newNo++
      out.push({ type: 'context', oldNo, newNo, text: a[i] })
      i++
      j++
    } else if (dp[i + 1][j] >= dp[i][j + 1]) {
      oldNo++
      out.push({ type: 'remove', oldNo, newNo: null, text: a[i] })
      i++
    } else {
      newNo++
      out.push({ type: 'add', oldNo: null, newNo, text: b[j] })
      j++
    }
  }
  while (i < a.length) {
    oldNo++
    out.push({ type: 'remove', oldNo, newNo: null, text: a[i] })
    i++
  }
  while (j < b.length) {
    newNo++
    out.push({ type: 'add', oldNo: null, newNo, text: b[j] })
    j++
  }
  return out
}

// contextRadius collapses unchanged stretches, unified-diff style, keeping N
// lines around each change. Hunks are only produced when there is at least
// one changed line.
export function toHunks(lines: DiffLine[], radius = 3): DiffHunk[] {
  const changeIdx: number[] = []
  lines.forEach((l, i) => {
    if (l.type !== 'context') changeIdx.push(i)
  })
  if (changeIdx.length === 0) return []

  const ranges: Array<[number, number]> = []
  for (const idx of changeIdx) {
    const lo = Math.max(0, idx - radius)
    const hi = Math.min(lines.length - 1, idx + radius)
    const last = ranges[ranges.length - 1]
    if (last && lo <= last[1] + 1) {
      last[1] = Math.max(last[1], hi)
    } else {
      ranges.push([lo, hi])
    }
  }
  return ranges.map(([lo, hi]) => ({ lines: lines.slice(lo, hi + 1) }))
}
