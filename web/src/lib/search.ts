import type { CodeSelection, ProjectSearchHit } from '../types'
import { selectionLabel } from './conversation'

/** Project search is a literal (or optional regex) scan of files on disk, not a semantic index or LSP. */
export const projectSearchCaption =
  '在当前项目内对磁盘上的文件内容做字面搜索（不是语义索引或语言服务）。忽略常见目录，跳过二进制和大文件，结果有数量上限。'

/** Must stay in line with the server context budget for a single selection. */
export const searchCitationMaxBytes = 16 * 1024

export type FileContentReader = (path: string) => Promise<{ content: string }>

export function hitLabel(hit: ProjectSearchHit): string {
  return selectionLabel({ path: hit.path, start_line: hit.line, end_line: hit.line })
}

export function draftLocateNotice(path: string, line: number): string {
  return `搜索结果来自磁盘上已保存的 ${path}。当前编辑器里有未保存草稿，已定位到第 ${line} 行——该行内容可能与搜索预览不同。`
}

/** 1-based line from a text file; a trailing newline does not add an extra line after the last content line in the usual split, except a final empty string. */
export function sourceLineAt(content: string, line: number): string | null {
  const lines = content.split('\n')
  if (line < 1 || line > lines.length) return null
  return lines[line - 1].replace(/\r$/, '')
}

/**
 * A search preview is a trimmed, possibly clipped and redacted snapshot of a
 * source line, not the line itself. True when the preview is still a faithful
 * prefix (or full trimmed form) of the current line.
 */
export function previewMatchesSourceLine(preview: string, line: string): boolean {
  const trimmed = line.trim()
  const p = preview.trim()
  if (p === trimmed) return true
  if (!p.endsWith('...')) return false
  const head = p.slice(0, -3)
  if (trimmed.startsWith(head)) return true
  // clipLine does TrimSpace(s[:200]) + "..."; compare that construction too.
  const first = trimmed.length > 200 ? trimmed.slice(0, 200).trim() : trimmed
  return first === head
}

export type SearchCitation = {
  selection: CodeSelection | null
  status: 'ok' | 'changed' | 'truncated' | 'missing' | 'error'
  notice: string
}

/**
 * Build a conversation citation from the current on-disk source line, not the
 * search preview. Detects a stale snapshot and will not invent text when the
 * line is gone.
 */
export async function citationFromSearchHit(hit: ProjectSearchHit, readFile: FileContentReader): Promise<SearchCitation> {
  const lineNo = Math.max(1, hit.line || 1)
  const label = hitLabel({ ...hit, line: lineNo })
  try {
    const file = await readFile(hit.path)
    const raw = sourceLineAt(file.content, lineNo)
    if (raw == null) {
      return {
        selection: null,
        status: 'missing',
        notice: `无法引用：${hit.path} 当前没有第 ${lineNo} 行（文件可能已变化）。搜索预览不能代替源行。`,
      }
    }
    const changed = !previewMatchesSourceLine(hit.preview, raw)
    let text = raw
    let truncated = false
    if (text.length > searchCitationMaxBytes) {
      text = text.slice(0, searchCitationMaxBytes)
      truncated = true
    }
    const selection: CodeSelection = {
      path: hit.path,
      start_line: lineNo,
      end_line: lineNo,
      text,
      draft: false,
    }
    if (truncated && changed) {
      return {
        selection,
        status: 'truncated',
        notice: `文件已变化，已改用当前磁盘 ${label}；该行超过引用长度，已截断。这不是搜索预览。`,
      }
    }
    if (truncated) {
      return {
        selection,
        status: 'truncated',
        notice: `已引用磁盘上 ${label} 的当前源行；该行超过引用长度，已截断。这不是搜索预览。`,
      }
    }
    if (changed) {
      return {
        selection,
        status: 'changed',
        notice: `文件已变化，已改用当前磁盘 ${label} 的源行，而不是搜索时的预览。`,
      }
    }
    return {
      selection,
      status: 'ok',
      notice: `已引用磁盘上 ${label} 的当前源行。`,
    }
  } catch (e) {
    const message = e instanceof Error ? e.message : String(e)
    return {
      selection: null,
      status: 'error',
      notice: `无法读取 ${hit.path} 的源行，未使用搜索预览代替：${message}`,
    }
  }
}
