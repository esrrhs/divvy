export type FileViewState = {
  sessionId: string | null
  path: string
  preferDiff?: boolean
  line?: number
  revealNonce?: number
}

export type FileNavRequest = Pick<FileViewState, 'path' | 'sessionId' | 'preferDiff' | 'line'>

/** Decide how opening a path/line interacts with an unsaved draft. */
export function planFileOpen(
  current: FileViewState | null,
  next: FileNavRequest,
  dirty: boolean,
): { kind: 'reveal' | 'open' | 'confirm'; view: FileViewState } {
  const target: FileViewState = {
    path: next.path,
    sessionId: next.sessionId,
    preferDiff: next.preferDiff,
    line: next.line,
    revealNonce: next.line ? 1 : 0,
  }
  if (current && current.path === target.path && current.sessionId === target.sessionId) {
    return {
      kind: 'reveal',
      view: {
        ...current,
        preferDiff: target.preferDiff ?? current.preferDiff,
        line: target.line ?? current.line,
        revealNonce: target.line ? (current.revealNonce ?? 0) + 1 : current.revealNonce,
      },
    }
  }
  if (dirty && current) {
    return { kind: 'confirm', view: target }
  }
  return { kind: 'open', view: target }
}

export const editorLineHeightPx = 12 * 1.6

export function scrollTopForLine(line: number, lineHeight = editorLineHeightPx): number {
  return Math.max(0, (Math.max(1, line) - 1) * lineHeight)
}
