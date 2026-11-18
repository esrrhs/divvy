/** @vitest-environment jsdom */

import { act } from 'react'
import { createRoot, type Root } from 'react-dom/client'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { ModelMarkdown, type CodeBlockActions } from './ModelMarkdown'
import type { FileRef } from '../types'

const RESULTS: FileRef[] = [
  { path: 'math/math.go', type: 'file' },
  { path: 'math/other.go', type: 'file' },
]

describe('ModelMarkdown code apply', () => {
  let root: Root
  let host: HTMLElement

  beforeEach(() => {
    host = document.createElement('div')
    document.body.appendChild(host)
    root = createRoot(host)
    ;(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true
  })

  afterEach(() => {
    act(() => root.unmount())
    host.remove()
    vi.restoreAllMocks()
  })

  function render(text: string, actions: CodeBlockActions) {
    act(() => root.render(<ModelMarkdown text={text} codeActions={actions} />))
  }

  function button(label: string | RegExp): HTMLButtonElement {
    const btn = [...host.querySelectorAll('button')].find((b) =>
      typeof label === 'string' ? b.textContent?.trim() === label : label.test(b.textContent ?? ''),
    )
    if (!btn) throw new Error(`button ${label} missing`)
    return btn
  }

  const CODE = 'func Add(a, b int) int {\n\treturn a + b\n}'

  it('shows the fence path hint and applies the block to the picked file', async () => {
    const search = vi.fn(async () => RESULTS)
    const write = vi.fn(async () => null)
    render(
      '```go math/math.go\n' + CODE + '\n```',
      { search, write },
    )
    // The hint rides in the header; the buttons are there.
    expect(host.textContent).toContain('math/math.go')
    expect(host.textContent).toContain('复制')
    expect(host.textContent).toContain('应用到文件…')

    await act(async () => {
      button('应用到文件…').dispatchEvent(new MouseEvent('click', { bubbles: true }))
    })
    await act(async () => {
      await new Promise((r) => setTimeout(r, 300))
    })
    // The hint seeded the search.
    expect(search).toHaveBeenCalledWith('math/math.go')
    expect(host.textContent).toContain('math/other.go')

    await act(async () => {
      button('math/math.go').dispatchEvent(new MouseEvent('click', { bubbles: true }))
    })
    expect(host.textContent).toContain('整体写入')

    await act(async () => {
      button('写入').dispatchEvent(new MouseEvent('click', { bubbles: true }))
    })
    expect(write).toHaveBeenCalledWith('math/math.go', CODE)
    expect(host.textContent).toContain('已写入 math/math.go')
  })

  it('offers writing a new file from a typed path', async () => {
    const search = vi.fn(async () => [] as FileRef[])
    const write = vi.fn(async () => null)
    render('```go\n' + CODE + '\n```', { search, write })

    await act(async () => {
      button('应用到文件…').dispatchEvent(new MouseEvent('click', { bubbles: true }))
    })
    await act(async () => {
      await new Promise((r) => setTimeout(r, 300))
    })
    expect(host.textContent).toContain('没有匹配文件')

    const input = host.querySelector('input') as HTMLInputElement
    await act(async () => {
      Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value')!.set!.call(input, 'scratch/notes.go')
      input.dispatchEvent(new Event('input', { bubbles: true }))
    })
    await act(async () => {
      button(/写入新文件/).dispatchEvent(new MouseEvent('click', { bubbles: true }))
    })
    await act(async () => {
      button('写入').dispatchEvent(new MouseEvent('click', { bubbles: true }))
    })
    expect(write).toHaveBeenCalledWith('scratch/notes.go', CODE)
  })

  it('surfaces write failures and keeps the picker open', async () => {
    const write = vi.fn(async () => '文件在打开之后已被外部修改，拒绝覆盖')
    render('```go\n' + CODE + '\n```', { search: vi.fn(async () => RESULTS), write })

    await act(async () => {
      button('应用到文件…').dispatchEvent(new MouseEvent('click', { bubbles: true }))
    })
    await act(async () => {
      await new Promise((r) => setTimeout(r, 300))
    })
    await act(async () => {
      button('math/math.go').dispatchEvent(new MouseEvent('click', { bubbles: true }))
    })
    await act(async () => {
      button('写入').dispatchEvent(new MouseEvent('click', { bubbles: true }))
    })
    expect(host.textContent).toContain('拒绝覆盖')
    // Still in confirm mode for a retry.
    expect(host.textContent).toContain('整体写入')
  })
})
