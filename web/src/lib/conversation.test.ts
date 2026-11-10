import { describe, expect, it } from 'vitest'
import {
  composeState,
  conversationRowKind,
  isResumablePhase,
  appliedStatusLabel,
  decisionsFromAccepts,
  initialHunkAccepts,
  mentionQuery,
  messageKindMeta,
  outcomeMeta,
  sameRelPath,
  shortWorkdir,
  lineRangeFromOffsets,
  selectionLabel,
} from './conversation'

describe('composeState', () => {
  it('allows starting when idle, done, or failed', () => {
    expect(composeState(undefined).kind).toBe('start')
    expect(composeState('done').kind).toBe('start')
    expect(composeState('failed').kind).toBe('start')
    expect(composeState('done').ok).toBe(true)
  })

  it('steers a follow-up into the current turn while a run is in progress', () => {
    const running = composeState('running')
    expect(running.ok).toBe(true)
    expect(running.kind).toBe('steer')
    if (running.ok && running.kind === 'steer') {
      expect(running.canTakeover).toBe(true)
      expect(running.canQueue).toBe(true)
      expect(running.primary).toMatch(/插入/)
      expect(running.hint).toMatch(/已完成/)
      expect(running.hint).toMatch(/立刻打断/)
      expect(running.hint).toMatch(/排队下一条/)
      expect(running.hint).toMatch(/中止并接管/)
    }
    expect(composeState('plan_review').kind).toBe('steer')
  })

  it('describes boundary inject for direct agent mode', () => {
    const running = composeState('running', false, 'direct')
    expect(running.ok).toBe(true)
    expect(running.kind).toBe('steer')
    if (running.ok && running.kind === 'steer') {
      expect(running.hint).toMatch(/下一模型步骤/)
      expect(running.hint).not.toMatch(/立刻打断当前叶子/)
      expect(running.canQueue).toBe(true)
      expect(running.canTakeover).toBe(true)
    }
  })

  it('keeps pause as resume, optional steer-on-resume, or takeover', () => {
    const paused = composeState('paused')
    expect(paused.ok).toBe(true)
    expect(paused.kind).toBe('paused')
    if (paused.ok && paused.kind === 'paused') {
      expect(paused.canTakeover).toBe(true)
      expect(paused.canQueue).toBe(false)
      expect(paused.hint).toMatch(/不会自动开跑/)
    }
  })

  it('blocks when a key must be re-entered after restart', () => {
    const s = composeState('done', true)
    expect(s.ok).toBe(false)
    expect(s.kind).toBe('blocked')
    if (!s.ok) expect(s.reason).toMatch(/API key/)
  })

  it('blocks compose when a pending rollback recovery is outstanding', () => {
    const s = composeState('done', false, 'direct', { message: '上次撤销未完成，已保留操作日志和备份' })
    expect(s.ok).toBe(false)
    expect(s.kind).toBe('blocked')
    if (!s.ok) expect(s.reason).toMatch(/撤销未完成/)
  })
})

describe('mentionQuery', () => {
  it('picks up an @query at the cursor', () => {
    const text = '请看 @src/he'
    expect(mentionQuery(text, text.length)).toEqual({ start: 3, query: 'src/he' })
    expect(mentionQuery('fix @', 5)).toEqual({ start: 4, query: '' })
  })

  it('ignores email-like tokens and queries broken by space', () => {
    expect(mentionQuery('a@b', 3)).toBeNull()
    expect(mentionQuery('see @foo bar', 12)).toBeNull()
    expect(mentionQuery('hello', 5)).toBeNull()
  })
})

describe('phase helpers', () => {
  it('treats only paused as resumable, not failed or done', () => {
    expect(isResumablePhase('paused')).toBe(true)
    expect(isResumablePhase('done')).toBe(false)
    expect(isResumablePhase('failed')).toBe(false)
    expect(isResumablePhase('running')).toBe(false)
  })

  it('classifies sidebar rows without calling a paused run active', () => {
    expect(conversationRowKind('paused')).toBe('paused')
    expect(conversationRowKind('running')).toBe('active')
    expect(conversationRowKind('plan_review')).toBe('active')
    expect(conversationRowKind(undefined)).toBe('idle')
  })
})

describe('message labels', () => {
  it('does not label execution status as a model reply', () => {
    expect(messageKindMeta('run_finished').label).toBe('执行结果')
    expect(messageKindMeta('run_started').label).toBe('执行状态')
    expect(messageKindMeta('user').label).toBe('你')
    expect(messageKindMeta('model_reply').label).toBe('模型回答')
    expect(messageKindMeta('tool_activity').label).toBe('工具')
    expect(messageKindMeta('ask').label).toBe('模型提问')
    expect(messageKindMeta('ask_answer').label).toBe('你的回答')
  })

  it('maps turn outcomes to concise result-card labels', () => {
    expect(outcomeMeta('completed').label).toBe('成功')
    expect(outcomeMeta('failed').label).toBe('失败')
    expect(outcomeMeta('aborted').label).toBe('已中止')
    expect(outcomeMeta('paused').label).toBe('已暂停')
  })
})

describe('shortWorkdir', () => {
  it('keeps a short path and ellipsizes a long one', () => {
    expect(shortWorkdir('/tmp/ws')).toBe('/tmp/ws')
    expect(shortWorkdir('/home/me/src/divvy')).toBe('.../src/divvy')
  })
})

describe('sameRelPath', () => {
  it('matches workdir-relative paths with optional ./ or slashes', () => {
    expect(sameRelPath('src/a.go', './src/a.go')).toBe(true)
    expect(sameRelPath('src/a.go', 'src/b.go')).toBe(false)
  })
})

describe('hunk decision helpers', () => {
  it('defaults unspecified hunks to accept and keeps stored rejects', () => {
    expect(initialHunkAccepts(3)).toEqual([true, true, true])
    expect(initialHunkAccepts(3, ['reject', 'accept'])).toEqual([false, true, true])
    expect(decisionsFromAccepts([true, false])).toEqual([
      { index: 0, accept: true },
      { index: 1, accept: false },
    ])
    expect(appliedStatusLabel('partial')).toBe('已部分接受')
    expect(appliedStatusLabel('rejected')).toBe('已全部拒绝')
    expect(appliedStatusLabel('rolled_back')).toBe('已随轮次撤销')
  })
})

describe('lineRangeFromOffsets', () => {
  const src = 'alpha\nbeta\ngamma\n'
  it('maps a mid-file span to 1-based lines', () => {
    expect(lineRangeFromOffsets(src, 6, 10)).toEqual({ start_line: 2, end_line: 2 })
    expect(lineRangeFromOffsets(src, 0, 10)).toEqual({ start_line: 1, end_line: 2 })
  })
  it('does not bump the end line when the selection stops on a newline', () => {
    expect(lineRangeFromOffsets(src, 0, 6)).toEqual({ start_line: 1, end_line: 1 })
  })
})

describe('selectionLabel', () => {
  it('includes the path, line range, and unsaved-draft marker', () => {
    expect(selectionLabel({ path: 'src/a.ts', start_line: 3, end_line: 5, draft: true })).toBe(
      'src/a.ts:L3-5（未保存草稿）',
    )
    expect(selectionLabel({ path: 'src/a.ts', start_line: 3, end_line: 3 })).toBe('src/a.ts:L3')
  })
})
