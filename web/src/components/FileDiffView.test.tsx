/** @vitest-environment jsdom */

import { act } from 'react'
import { createRoot, type Root } from 'react-dom/client'
import { afterEach, beforeEach, describe, expect, it } from 'vitest'
import type { FileChange } from '../types'
import { FileDiffList } from './FileDiffView'

const OLD_GO = `package calc

// Add returns the sum of a and b.
func Add(a, b int) int {
	return a - b
}
`

const NEW_GO = `package calc

// Add returns the sum of a and b.
func Add(a, b int) int {
	return a + b
}
`

describe('FileDiffList', () => {
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
  })

  it('colours the reviewed code, without changing the text', async () => {
    const change: FileChange = {
      path: 'calc.go',
      status: 'modified',
      old_content: OLD_GO,
      new_content: NEW_GO,
    }
    await act(async () => {
      root.render(<FileDiffList changes={[change]} />)
      await Promise.resolve()
    })
    // A modified file starts collapsed: open it the way the user does.
    const header = host.querySelector('button')
    expect(header).toBeTruthy()
    await act(async () => {
      ;(header as HTMLButtonElement).click()
      await Promise.resolve()
    })

    const text = host.textContent ?? ''
    // The code is still exactly the code.
    expect(text).toContain('return a + b')
    expect(text).toContain('return a - b')
    // And it is now coloured: a Go keyword carries a keyword class.
    const keyword = host.querySelector('.text-sky-300')
    expect(keyword).toBeTruthy()
    expect(keyword?.textContent).toBe('func')
  })

  it('leaves an unknown language uncoloured', async () => {
    const change: FileChange = {
      path: 'notes.txt',
      status: 'modified',
      old_content: 'func Add(a, b int) int {\n\treturn a - b\n}\n',
      new_content: 'func Add(a, b int) int {\n\treturn a + b\n}\n',
    }
    await act(async () => {
      root.render(<FileDiffList changes={[change]} />)
      await Promise.resolve()
    })
    const header = host.querySelector('button')
    await act(async () => {
      ;(header as HTMLButtonElement).click()
      await Promise.resolve()
    })
    expect(host.querySelector('.text-sky-300')).toBeNull()
    expect(host.textContent).toContain('return a + b')
  })
})
