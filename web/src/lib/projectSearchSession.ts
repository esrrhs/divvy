import type { ProjectSearchHit } from '../types'

/** Identity of the project/conversation a search result belongs to. */
export type SearchScope = {
  conversationId: string | null
  sessionId: string | null
}

export function sameSearchScope(a: SearchScope, b: SearchScope): boolean {
  return (a.conversationId ?? '') === (b.conversationId ?? '') && (a.sessionId ?? '') === (b.sessionId ?? '')
}

export type ProjectSearchView = {
  generation: number
  scope: SearchScope
  hits: ProjectSearchHit[] | null
  loading: boolean
  error: string | null
  truncated: boolean
  scanned: number
}

export function emptySearchView(scope: SearchScope, generation = 0): ProjectSearchView {
  return { generation, scope, hits: null, loading: false, error: null, truncated: false, scanned: 0 }
}

/**
 * Drop in-flight results and visible hits immediately when the query, options,
 * or conversation/session scope change. The new generation token is what late
 * responses are checked against.
 */
export function invalidateSearch(_view: ProjectSearchView, scope: SearchScope, generation: number): ProjectSearchView {
  return emptySearchView(scope, generation)
}

export function beginSearch(view: ProjectSearchView, token: number, scope: SearchScope): ProjectSearchView {
  if (view.generation !== token || !sameSearchScope(view.scope, scope)) return view
  return { ...view, loading: true, error: null }
}

export function applySearchResult(
  view: ProjectSearchView,
  token: number,
  scope: SearchScope,
  result: { hits: ProjectSearchHit[]; truncated: boolean; scanned: number } | { error: string },
): ProjectSearchView {
  if (view.generation !== token || !sameSearchScope(view.scope, scope)) return view
  if ('error' in result) {
    return { ...view, loading: false, hits: null, error: result.error, truncated: false, scanned: 0 }
  }
  return {
    ...view,
    loading: false,
    hits: result.hits,
    truncated: result.truncated,
    scanned: result.scanned,
    error: null,
  }
}

/** Hits may be opened or cited only while they still belong to the live scope. */
export function canActOnSearchHits(view: ProjectSearchView, current: SearchScope): boolean {
  return !!view.hits && view.hits.length > 0 && !view.loading && sameSearchScope(view.scope, current)
}
