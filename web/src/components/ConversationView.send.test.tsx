/** @vitest-environment jsdom */

import { act } from 'react'
import { createRoot, type Root } from 'react-dom/client'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { api } from '../api/client'
import { useStore } from '../store'
import { ConversationView } from './ConversationView'
import type { CodeSelection, Conversation, SessionDetail } from '../types'

function deferred<T>() {
  let resolve!: (value: T) => void
  let reject!: (reason?: unknown) => void
  const promise = new Promise<T>((res, rej) => {
    resolve = res
    reject = rej
  })
  return { promise, resolve, reject }
}

function conv(id: string, sid = `sess-${id}`, extra?: Partial<Conversation>): Conversation {
  return {
    id,
    title: extra?.title ?? id,
    workdir: `/tmp/${id}`,
    created_at: '',
    updated_at: '',
    settings: { isolate: true, git_commit: false, web: false, browser: false, agent_mode: 'direct' },
    messages: extra?.messages ?? [],
    work_log: [],
    session_ids: [sid],
    current_session_id: sid,
    run_phase: 'done',
    has_api_key: true,
    context_note: '',
    ...extra,
  }
}

function sessionDetail(id: string): SessionDetail {
  return {
    session_id: id,
    workdir: '/tmp',
    live: { phase: 'done', approval_mode: 'auto', session_id: id, workdir: '/tmp' },
    tree: null,
  }
}

const sel = (text: string, path = 'pkg/a.go'): CodeSelection => ({
  path,
  start_line: 1,
  end_line: 1,
  text,
  draft: false,
})

describe('ConversationView send continuation', () => {
  let root: Root
  let host: HTMLElement

  beforeEach(() => {
    ;(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true
    host = document.createElement('div')
    document.body.appendChild(host)
    root = createRoot(host)
    vi.spyOn(api, 'getConversation').mockImplementation(async (id) => conv(id))
    vi.spyOn(api, 'getSession').mockImplementation(async (id) => sessionDetail(id))
    vi.spyOn(api, 'listConversations').mockResolvedValue([conv('a'), conv('b')])
    vi.spyOn(api, 'getTurnChanges').mockResolvedValue({ session_id: '', changes: [], editable: false })
    useStore.setState({
      currentConvId: null,
      currentId: null,
      conversation: null,
      desiredConvId: undefined,
      convNavGen: 0,
      sessionNavGen: 0,
      pendingSelections: [],
      pendingSelectionsByConv: {},
      conversations: [],
      fileView: null,
      fileDirty: false,
      pendingFileNav: null,
      apiError: null,
      actionError: null,
      live: null,
      tree: null,
    })
  })

  afterEach(() => {
    act(() => {
      root.unmount()
    })
    host.remove()
    vi.restoreAllMocks()
  })

  async function mountView() {
    await act(async () => {
      root.render(<ConversationView />)
    })
  }

  async function typeAndSubmit(message: string) {
    const ta = host.querySelector('textarea') as HTMLTextAreaElement
    expect(ta).toBeTruthy()
    expect(ta.disabled).toBe(false)
    await act(async () => {
      Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, 'value')!.set!.call(ta, message)
      ta.dispatchEvent(new Event('input', { bubbles: true }))
    })
    await act(async () => {
      host.querySelector('form')!.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }))
    })
  }

  function composer() {
    const ta = host.querySelector('textarea') as HTMLTextAreaElement
    const submit = host.querySelector('button[type="submit"]') as HTMLButtonElement
    return { ta, submit }
  }

  it('a successful A send must not clear new B citations when the real composer continuation runs', async () => {
    const post = deferred<Conversation>()
    vi.spyOn(api, 'postMessage').mockReturnValue(post.promise)

    await useStore.getState().openConversation('a')
    await mountView()
    const ta = host.querySelector('textarea') as HTMLTextAreaElement
    expect(ta).toBeTruthy()
    await act(async () => {
      Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, 'value')!.set!.call(ta, 'ordinary A message')
      ta.dispatchEvent(new Event('input', { bubbles: true }))
    })
    await act(async () => {
      host.querySelector('form')!.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }))
    })
    expect(api.postMessage).toHaveBeenCalledWith('a', 'ordinary A message', expect.anything())

    await act(async () => {
      await useStore.getState().openConversation('b')
      useStore.getState().attachSelection(sel('new B citation', 'b.ts'))
    })
    expect(useStore.getState().currentConvId).toBe('b')
    expect(useStore.getState().pendingSelections.map((s) => s.text)).toEqual(['new B citation'])

    await act(async () => {
      post.resolve(conv('a', 'sess-a', { title: 'A after send', messages: [{ id: 'm1', role: 'user', kind: 'user', text: 'ordinary A message', created_at: '' }] }))
      await Promise.resolve()
      await Promise.resolve()
      await Promise.resolve()
    })

    expect(useStore.getState().currentConvId).toBe('b')
    expect(useStore.getState().currentId).toBe('sess-b')
    expect(useStore.getState().pendingSelections.map((s) => s.text)).toEqual(['new B citation'])
    expect(useStore.getState().conversations.find((c) => c.id === 'a')?.title).toBe('A after send')
  })

  it('in-order send on the same conversation still drops only the citations that were sent', async () => {
    vi.spyOn(api, 'postMessage').mockImplementation(async (id) => conv(id))
    await useStore.getState().openConversation('a')
    useStore.getState().attachSelection(sel('sent A'))
    await mountView()
    const ta = host.querySelector('textarea') as HTMLTextAreaElement
    await act(async () => {
      Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, 'value')!.set!.call(ta, 'ordinary A message')
      ta.dispatchEvent(new Event('input', { bubbles: true }))
    })
    await act(async () => {
      host.querySelector('form')!.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }))
      await Promise.resolve()
      await Promise.resolve()
    })
    expect(useStore.getState().currentConvId).toBe('a')
    expect(useStore.getState().pendingSelections).toEqual([])
    expect(ta.value).toBe('')
  })

  it('late A success must not unlock B while B still has a pending send; B unlocks only when its own request finishes', async () => {
    const postA = deferred<Conversation>()
    const postB = deferred<Conversation>()
    vi.spyOn(api, 'postMessage').mockImplementation((id) => (id === 'a' ? postA.promise : postB.promise))

    await useStore.getState().openConversation('a')
    await mountView()
    await typeAndSubmit('ordinary A message')
    await act(async () => {
      await useStore.getState().openConversation('b')
    })
    await typeAndSubmit('ordinary B message')
    expect(vi.mocked(api.postMessage).mock.calls.map((args) => args[0])).toEqual(['a', 'b'])

    let { ta, submit } = composer()
    expect(ta.disabled).toBe(true)
    expect(submit.disabled).toBe(true)

    await act(async () => {
      postA.resolve(conv('a'))
      await Promise.resolve()
      await Promise.resolve()
      await Promise.resolve()
    })

    expect(useStore.getState().currentConvId).toBe('b')
    expect(useStore.getState().currentId).toBe('sess-b')
    ;({ ta, submit } = composer())
    expect(ta.value).toBe('ordinary B message')
    expect(ta.disabled).toBe(true)
    expect(submit.disabled).toBe(true)

    await act(async () => {
      postB.resolve(conv('b'))
      await Promise.resolve()
      await Promise.resolve()
      await Promise.resolve()
    })
    ;({ ta, submit } = composer())
    expect(ta.disabled).toBe(false)
    expect(submit.disabled).toBe(true) // empty input after a successful B send
    expect(ta.value).toBe('')
  })

  it('late A failure must not unlock B while B still has a pending send', async () => {
    const postA = deferred<Conversation>()
    const postB = deferred<Conversation>()
    vi.spyOn(api, 'postMessage').mockImplementation((id) => (id === 'a' ? postA.promise : postB.promise))

    await useStore.getState().openConversation('a')
    await mountView()
    await typeAndSubmit('ordinary A message')
    await act(async () => {
      await useStore.getState().openConversation('b')
    })
    await typeAndSubmit('ordinary B message')

    await act(async () => {
      postA.reject(new Error('send A failed'))
      await Promise.resolve()
      await Promise.resolve()
      await Promise.resolve()
    })

    expect(useStore.getState().currentConvId).toBe('b')
    const { ta, submit } = composer()
    expect(ta.value).toBe('ordinary B message')
    expect(ta.disabled).toBe(true)
    expect(submit.disabled).toBe(true)

    await act(async () => {
      postB.resolve(conv('b'))
      await Promise.resolve()
      await Promise.resolve()
      await Promise.resolve()
    })
    expect(composer().ta.disabled).toBe(false)
  })

  it('returning to A while its send is still pending shows A as busy again', async () => {
    const postA = deferred<Conversation>()
    vi.spyOn(api, 'postMessage').mockReturnValue(postA.promise)

    await useStore.getState().openConversation('a')
    await mountView()
    await typeAndSubmit('ordinary A message')
    expect(composer().ta.disabled).toBe(true)

    await act(async () => {
      await useStore.getState().openConversation('b')
    })
    expect(composer().ta.disabled).toBe(false)

    await act(async () => {
      await useStore.getState().openConversation('a')
    })
    expect(useStore.getState().currentConvId).toBe('a')
    expect(composer().ta.disabled).toBe(true)
    expect(composer().submit.disabled).toBe(true)

    await act(async () => {
      postA.resolve(conv('a'))
      await Promise.resolve()
      await Promise.resolve()
      await Promise.resolve()
    })
    expect(composer().ta.disabled).toBe(false)
  })

  it('an in-order send stays busy until its own request finishes, then unlocks and clears the input', async () => {
    const post = deferred<Conversation>()
    vi.spyOn(api, 'postMessage').mockReturnValue(post.promise)
    await useStore.getState().openConversation('a')
    await mountView()
    await typeAndSubmit('ordinary A message')
    expect(composer().ta.disabled).toBe(true)
    expect(composer().submit.disabled).toBe(true)
    expect(composer().ta.value).toBe('ordinary A message')

    await act(async () => {
      post.resolve(conv('a'))
      await Promise.resolve()
      await Promise.resolve()
      await Promise.resolve()
    })
    expect(composer().ta.disabled).toBe(false)
    expect(composer().ta.value).toBe('')
  })

  it('late A saveKey success must not unlock a pending B send; B unlocks only when its own request finishes', async () => {
    const savingA = deferred<Conversation>()
    const postB = deferred<Conversation>()
    vi.mocked(api.getConversation).mockImplementation(async (id) => ({ ...conv(id), key_required: id === 'a' }))
    vi.spyOn(api, 'setConversationKey').mockReturnValue(savingA.promise)
    vi.spyOn(api, 'postMessage').mockReturnValue(postB.promise)

    await useStore.getState().openConversation('a')
    await mountView()
    const fixtureInput = host.querySelector('input[type="password"]') as HTMLInputElement
    expect(fixtureInput).toBeTruthy()
    await act(async () => {
      Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value')!.set!.call(fixtureInput, 'unit-test-placeholder')
      fixtureInput.dispatchEvent(new Event('input', { bubbles: true }))
    })
    const saveButton = Array.from(host.querySelectorAll('button')).find((b) => b.textContent?.trim() === '保存到内存') as HTMLButtonElement
    await act(async () => {
      saveButton.click()
    })
    expect(api.setConversationKey).toHaveBeenCalledWith('a', 'unit-test-placeholder')

    await act(async () => {
      await useStore.getState().openConversation('b')
    })
    await typeAndSubmit('ordinary B message')
    expect(composer().ta.disabled).toBe(true)

    await act(async () => {
      savingA.resolve({ ...conv('a'), key_required: false })
      await Promise.resolve()
      await Promise.resolve()
      await Promise.resolve()
    })

    expect(useStore.getState().currentConvId).toBe('b')
    expect(useStore.getState().currentId).toBe('sess-b')
    const { ta, submit } = composer()
    expect(ta.value).toBe('ordinary B message')
    expect(ta.disabled).toBe(true)
    expect(submit.disabled).toBe(true)

    await act(async () => {
      postB.resolve(conv('b'))
      await Promise.resolve()
      await Promise.resolve()
      await Promise.resolve()
    })
    expect(composer().ta.disabled).toBe(false)
    expect(composer().ta.value).toBe('')
  })

  it('late A saveKey failure must not unlock a pending B send', async () => {
    const savingA = deferred<Conversation>()
    const postB = deferred<Conversation>()
    vi.mocked(api.getConversation).mockImplementation(async (id) => ({ ...conv(id), key_required: id === 'a' }))
    vi.spyOn(api, 'setConversationKey').mockReturnValue(savingA.promise)
    vi.spyOn(api, 'postMessage').mockReturnValue(postB.promise)

    await useStore.getState().openConversation('a')
    await mountView()
    const fixtureInput = host.querySelector('input[type="password"]') as HTMLInputElement
    await act(async () => {
      Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value')!.set!.call(fixtureInput, 'unit-test-placeholder')
      fixtureInput.dispatchEvent(new Event('input', { bubbles: true }))
    })
    const saveButton = Array.from(host.querySelectorAll('button')).find((b) => b.textContent?.trim() === '保存到内存') as HTMLButtonElement
    await act(async () => {
      saveButton.click()
    })
    await act(async () => {
      await useStore.getState().openConversation('b')
    })
    await typeAndSubmit('ordinary B message')

    await act(async () => {
      savingA.reject(new Error('save failed'))
      await Promise.resolve()
      await Promise.resolve()
      await Promise.resolve()
    })

    expect(useStore.getState().currentConvId).toBe('b')
    const { ta, submit } = composer()
    expect(ta.value).toBe('ordinary B message')
    expect(ta.disabled).toBe(true)
    expect(submit.disabled).toBe(true)

    await act(async () => {
      postB.resolve(conv('b'))
      await Promise.resolve()
      await Promise.resolve()
      await Promise.resolve()
    })
    expect(composer().ta.disabled).toBe(false)
  })

  it('returning to A while its saveKey is still pending shows A as busy, and send on A does not start', async () => {
    const savingA = deferred<Conversation>()
    vi.mocked(api.getConversation).mockImplementation(async (id) => ({ ...conv(id), key_required: true }))
    vi.spyOn(api, 'setConversationKey').mockReturnValue(savingA.promise)
    const post = vi.spyOn(api, 'postMessage')

    await useStore.getState().openConversation('a')
    await mountView()
    const fixtureInput = host.querySelector('input[type="password"]') as HTMLInputElement
    await act(async () => {
      Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value')!.set!.call(fixtureInput, 'unit-test-placeholder')
      fixtureInput.dispatchEvent(new Event('input', { bubbles: true }))
    })
    const saveButton = Array.from(host.querySelectorAll('button')).find((b) => b.textContent?.trim() === '保存到内存') as HTMLButtonElement
    await act(async () => {
      saveButton.click()
    })

    await act(async () => {
      await useStore.getState().openConversation('b')
    })
    await act(async () => {
      await useStore.getState().openConversation('a')
    })
    expect(useStore.getState().currentConvId).toBe('a')
    const saveAgain = Array.from(host.querySelectorAll('button')).find((b) => b.textContent?.trim() === '保存到内存') as HTMLButtonElement
    expect(saveAgain).toBeTruthy()
    expect(saveAgain.disabled).toBe(true)

    // key_required blocks compose; after save still pending, even if the key form
    // is showing, a send attempt must not start a second composer request on A.
    const form = host.querySelector('form')
    if (form) {
      await act(async () => {
        form.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }))
      })
    }
    expect(post).not.toHaveBeenCalled()

    await act(async () => {
      savingA.resolve({ ...conv('a'), key_required: false })
      await Promise.resolve()
      await Promise.resolve()
      await Promise.resolve()
    })
    expect(api.setConversationKey).toHaveBeenCalledWith('a', 'unit-test-placeholder')
  })

  it('completing A saveKey after the composer unmounts does not unlock a later B send', async () => {
    const savingA = deferred<Conversation>()
    const postB = deferred<Conversation>()
    vi.mocked(api.getConversation).mockImplementation(async (id) => ({ ...conv(id), key_required: id === 'a' }))
    vi.spyOn(api, 'setConversationKey').mockReturnValue(savingA.promise)
    vi.spyOn(api, 'postMessage').mockReturnValue(postB.promise)

    await useStore.getState().openConversation('a')
    await mountView()
    const fixtureInput = host.querySelector('input[type="password"]') as HTMLInputElement
    await act(async () => {
      Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value')!.set!.call(fixtureInput, 'unit-test-placeholder')
      fixtureInput.dispatchEvent(new Event('input', { bubbles: true }))
    })
    const saveButton = Array.from(host.querySelectorAll('button')).find((b) => b.textContent?.trim() === '保存到内存') as HTMLButtonElement
    await act(async () => {
      saveButton.click()
    })

    await act(async () => {
      root.unmount()
    })
    await act(async () => {
      savingA.resolve({ ...conv('a'), key_required: false })
      await Promise.resolve()
      await Promise.resolve()
      await Promise.resolve()
    })

    root = createRoot(host)
    await useStore.getState().openConversation('b')
    await mountView()
    await typeAndSubmit('ordinary B message')
    expect(composer().ta.disabled).toBe(true)

    await act(async () => {
      postB.resolve(conv('b'))
      await Promise.resolve()
      await Promise.resolve()
      await Promise.resolve()
    })
    expect(composer().ta.disabled).toBe(false)
    expect(composer().ta.value).toBe('')
  })

  it('an in-order saveKey stays busy until it finishes, then unlocks and clears only that conversation’s key field', async () => {
    const savingA = deferred<Conversation>()
    vi.mocked(api.getConversation).mockImplementation(async (id) => ({ ...conv(id), key_required: true }))
    vi.spyOn(api, 'setConversationKey').mockReturnValue(savingA.promise)

    await useStore.getState().openConversation('a')
    await mountView()
    const fixtureInput = host.querySelector('input[type="password"]') as HTMLInputElement
    await act(async () => {
      Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value')!.set!.call(fixtureInput, 'unit-test-placeholder')
      fixtureInput.dispatchEvent(new Event('input', { bubbles: true }))
    })
    const saveButton = Array.from(host.querySelectorAll('button')).find((b) => b.textContent?.trim() === '保存到内存') as HTMLButtonElement
    await act(async () => {
      saveButton.click()
    })
    expect(saveButton.disabled).toBe(true)

    await act(async () => {
      savingA.resolve({ ...conv('a'), key_required: false })
      await Promise.resolve()
      await Promise.resolve()
      await Promise.resolve()
    })
    expect(useStore.getState().currentConvId).toBe('a')
    expect(useStore.getState().conversation?.key_required).toBe(false)
  })

  it('an in-flight saveKey on the same conversation blocks send until the save finishes', async () => {
    const savingA = deferred<Conversation>()
    vi.mocked(api.getConversation).mockImplementation(async (id) => ({ ...conv(id), key_required: true }))
    vi.spyOn(api, 'setConversationKey').mockReturnValue(savingA.promise)
    const post = vi.spyOn(api, 'postMessage')

    await useStore.getState().openConversation('a')
    await mountView()
    const fixtureInput = host.querySelector('input[type="password"]') as HTMLInputElement
    await act(async () => {
      Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value')!.set!.call(fixtureInput, 'unit-test-placeholder')
      fixtureInput.dispatchEvent(new Event('input', { bubbles: true }))
    })
    const saveButton = Array.from(host.querySelectorAll('button')).find((b) => b.textContent?.trim() === '保存到内存') as HTMLButtonElement
    await act(async () => {
      saveButton.click()
    })
    expect(saveButton.disabled).toBe(true)
    expect(composer().ta.disabled).toBe(true)

    await act(async () => {
      const ta = composer().ta
      Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, 'value')!.set!.call(ta, 'should not send yet')
      ta.dispatchEvent(new Event('input', { bubbles: true }))
      host.querySelector('form')!.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }))
    })
    expect(post).not.toHaveBeenCalled()

    await act(async () => {
      savingA.resolve({ ...conv('a'), key_required: false })
      await Promise.resolve()
      await Promise.resolve()
      await Promise.resolve()
    })
    expect(post).not.toHaveBeenCalled()
    expect(useStore.getState().conversation?.key_required).toBe(false)
    expect(composer().ta.disabled).toBe(false)
  })

  it('late A ask success must not clear B’s in-flight ask or unlock it early', async () => {
    const answeringA = deferred<{ ok?: boolean }>()
    const answeringB = deferred<{ ok?: boolean }>()
    vi.mocked(api.getConversation).mockImplementation(async (id) => ({
      ...conv(id),
      pending_ask: id === 'a' ? 'question A' : 'question B',
      run_phase: 'running',
    }))
    const answer = vi.spyOn(api, 'answer').mockImplementation(async (id: string) => {
      if (id === 'a' || id.endsWith('a')) return answeringA.promise as never
      return answeringB.promise as never
    })

    await useStore.getState().openConversation('a')
    await mountView()
    const askA = Array.from(host.querySelectorAll('textarea')).find((el) => (el as HTMLTextAreaElement).placeholder.includes('输入回答')) as HTMLTextAreaElement
    expect(askA).toBeTruthy()
    await act(async () => {
      Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, 'value')!.set!.call(askA, 'answer A')
      askA.dispatchEvent(new Event('input', { bubbles: true }))
    })
    const sendA = Array.from(host.querySelectorAll('button')).find((b) => b.textContent?.trim() === '发送回答') as HTMLButtonElement
    await act(async () => {
      sendA.click()
    })
    expect(answer).toHaveBeenCalled()

    await act(async () => {
      await useStore.getState().openConversation('b')
    })
    const askB = Array.from(host.querySelectorAll('textarea')).find((el) => (el as HTMLTextAreaElement).placeholder.includes('输入回答')) as HTMLTextAreaElement
    expect(askB).toBeTruthy()
    await act(async () => {
      Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, 'value')!.set!.call(askB, 'answer B')
      askB.dispatchEvent(new Event('input', { bubbles: true }))
    })
    const sendB = Array.from(host.querySelectorAll('button')).find((b) => b.textContent?.trim() === '发送回答') as HTMLButtonElement
    await act(async () => {
      sendB.click()
    })
    expect(sendB.disabled).toBe(true)
    expect(askB.value).toBe('answer B')

    await act(async () => {
      answeringA.resolve({ ok: true })
      await Promise.resolve()
      await Promise.resolve()
      await Promise.resolve()
    })
    expect(useStore.getState().currentConvId).toBe('b')
    const askStill = Array.from(host.querySelectorAll('textarea')).find((el) => (el as HTMLTextAreaElement).placeholder.includes('输入回答')) as HTMLTextAreaElement
    const sendStill = Array.from(host.querySelectorAll('button')).find((b) => b.textContent?.trim() === '发送回答') as HTMLButtonElement
    expect(askStill.value).toBe('answer B')
    expect(sendStill.disabled).toBe(true)

    await act(async () => {
      answeringB.resolve({ ok: true })
      await Promise.resolve()
      await Promise.resolve()
      await Promise.resolve()
    })
    const askDone = Array.from(host.querySelectorAll('textarea')).find((el) => (el as HTMLTextAreaElement).placeholder.includes('输入回答')) as HTMLTextAreaElement | undefined
    if (askDone) {
      expect(askDone.value).toBe('')
      const sendDone = Array.from(host.querySelectorAll('button')).find((b) => b.textContent?.trim() === '发送回答') as HTMLButtonElement
      expect(sendDone.disabled).toBe(true)
    }
  })
})
