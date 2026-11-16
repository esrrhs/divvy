/** @vitest-environment jsdom */

import { act } from 'react'
import { createRoot, type Root } from 'react-dom/client'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { ErrorBoundary } from './ErrorBoundary'

// A render error used to unmount the whole app into a silent white screen.
// The boundary must show what broke and keep a way back.
let broken = true
function Boom(): never {
  throw new Error('渲染炸了')
}
function MaybeBoom() {
  if (broken) throw new Error('渲染炸了')
  return <div>已恢复</div>
}

describe('ErrorBoundary', () => {
  let root: Root
  let host: HTMLElement

  beforeEach(() => {
    ;(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true
    vi.spyOn(console, 'error').mockImplementation(() => {})
    host = document.createElement('div')
    document.body.appendChild(host)
    root = createRoot(host)
  })

  afterEach(() => {
    act(() => {
      root.unmount()
    })
    host.remove()
    vi.restoreAllMocks()
  })

  it('shows the error and a recovery path instead of unmounting everything', () => {
    act(() => {
      root.render(
        <ErrorBoundary>
          <Boom />
        </ErrorBoundary>,
      )
    })
    expect(host.textContent).toContain('界面渲染出错了')
    expect(host.textContent).toContain('渲染炸了')
    const reload = host.querySelector('button')?.textContent
    expect(reload).toBe('重载页面')
  })

  it('尝试恢复 resets the state so a fixed child renders again', () => {
    broken = true
    act(() => {
      root.render(
        <ErrorBoundary>
          <MaybeBoom />
        </ErrorBoundary>,
      )
    })
    expect(host.textContent).toContain('界面渲染出错了')
    broken = false
    const retry = [...host.querySelectorAll('button')].find((b) => b.textContent === '尝试恢复')
    expect(retry).toBeTruthy()
    act(() => {
      retry!.click()
    })
    // The boundary retried the (now honest) children — no fallback text.
    expect(host.textContent).toContain('已恢复')
    expect(host.textContent).not.toContain('界面渲染出错了')
  })

  it('renders children untouched when nothing throws', () => {
    act(() => {
      root.render(
        <ErrorBoundary>
          <div>正常内容</div>
        </ErrorBoundary>,
      )
    })
    expect(host.textContent).toContain('正常内容')
  })
})
