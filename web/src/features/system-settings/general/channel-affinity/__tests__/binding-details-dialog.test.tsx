import assert from 'node:assert/strict'
import { after, afterEach, test } from 'node:test'

import { Window } from 'happy-dom'

const dom = new Window()
for (const key of [
  'window',
  'document',
  'navigator',
  'HTMLElement',
  'HTMLButtonElement',
  'SVGElement',
  'Node',
  'Element',
  'Event',
  'MutationObserver',
  'ResizeObserver',
  'requestAnimationFrame',
  'cancelAnimationFrame',
  'getComputedStyle',
] as const) {
  Object.defineProperty(globalThis, key, {
    configurable: true,
    value: dom[key],
  })
}
;(
  globalThis as typeof globalThis & { IS_REACT_ACT_ENVIRONMENT?: boolean }
).IS_REACT_ACT_ENVIRONMENT = true
const { act } = await import('react')
const { createRoot } = await import('react-dom/client')
const { createInstance } = await import('i18next')
const { I18nextProvider } = await import('react-i18next')
const { QueryClient, QueryClientProvider } = await import('@tanstack/react-query')
const { api } = await import('@/lib/api')
const { BindingDetailsDialog } = await import('../binding-details-dialog')
const originalAdapter = api.defaults.adapter
after(() => dom.close())

async function render(open = true) {
  const host = document.createElement('div')
  document.body.append(host)
  const root = createRoot(host)
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  const i18n = createInstance()
  await i18n.init({ lng: 'en', resources: {} })
  await act(async () =>
    root.render(
      <QueryClientProvider client={queryClient}>
        <I18nextProvider i18n={i18n}>
          <BindingDetailsDialog open={open} onOpenChange={() => undefined} />
        </I18nextProvider>
      </QueryClientProvider>
    )
  )
  return { host, root }
}

afterEach(() => {
  api.defaults.adapter = originalAdapter
  document.body.replaceChildren()
})

function deferred<T>() {
  let resolve!: (value: T) => void
  let reject!: (error: unknown) => void
  const promise = new Promise<T>((res, rej) => {
    resolve = res
    reject = rej
  })
  return { promise, resolve, reject }
}

async function flushPromises() {
  for (let i = 0; i < 6; i++) await Promise.resolve()
}

async function waitForText(text: string) {
  for (let i = 0; i < 50; i++) {
    if ((document.body.textContent ?? '').includes(text)) return
    await new Promise<void>((resolve) => setTimeout(resolve, 0))
  }
  assert.fail(`Timed out waiting for ${text}`)
}

test(
  'rendersLoadingThenAggregatedBindings',
  { concurrency: false },
  async () => {
    const original = api.defaults.adapter
    const response = deferred<unknown>()
    const started = deferred<void>()
    api.defaults.adapter = (() => {
      started.resolve()
      return response.promise
    }) as typeof api.defaults.adapter
    const { host, root } = await render()
    await act(async () => started.promise)
    assert.match(document.body.textContent ?? '', /Loading/)
    response.resolve({
      data: {
        success: true,
        data: [
          {
            channel_id: 2,
            channel_name: '备用',
            tokens: [
              { token_id: 102, token_name: '备' },
              { token_id: 101, token_name: '主' },
            ],
          },
          {
            channel_id: 1,
            channel_name: '主渠道',
            tokens: [{ token_id: 3, token_name: '令牌' }],
          },
        ],
      },
      status: 200,
      statusText: 'OK',
      headers: {},
      config: {},
    } as never)
    await act(async () => {
      await response.promise
      await flushPromises()
    })
    await act(async () => {
      await waitForText('主渠道')
      await waitForText('备（102）')
    })
    await act(async () => root.unmount())
    host.remove()
    api.defaults.adapter = original
  }
)

test('rendersEmptyStateWhenNoBindings', { concurrency: false }, async () => {
  const original = api.defaults.adapter
  const response = deferred<unknown>()
  const started = deferred<void>()
  api.defaults.adapter = (() => {
    started.resolve()
    return response.promise
  }) as typeof api.defaults.adapter
  const { host, root } = await render()
  await act(async () => started.promise)
  response.resolve({
    data: { success: true, data: [] },
    status: 200,
    statusText: 'OK',
    headers: {},
    config: {},
  } as never)
  await act(async () => {
    await response.promise
    await flushPromises()
  })
  await act(async () => waitForText('No bindings'))
  await act(async () => root.unmount())
  host.remove()
  api.defaults.adapter = original
})

test('rendersErrorStateWhenRequestFails', { concurrency: false }, async () => {
  const original = api.defaults.adapter
  const response = deferred<never>()
  const started = deferred<void>()
  api.defaults.adapter = (() => {
    started.resolve()
    return response.promise
  }) as typeof api.defaults.adapter
  const { host, root } = await render()
  await act(async () => started.promise)
  response.reject(new Error('failed'))
  await act(async () => {
    await response.promise.catch(() => undefined)
    await flushPromises()
  })
  await act(async () => waitForText('Failed to load bindings'))
  await act(async () => root.unmount())
  host.remove()
  api.defaults.adapter = original
})

test(
  'closesDialogWithoutChangingCacheStats',
  { concurrency: false },
  async () => {
    let calls = 0
    const original = api.defaults.adapter
    api.defaults.adapter = async (config) => {
      calls++
      return {
        data: { success: true, data: [] },
        status: 200,
        statusText: 'OK',
        headers: {},
        config,
      }
    }
    const changes: boolean[] = []
    const host = document.createElement('div')
    document.body.append(host)
    const root = createRoot(host)
    const queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    })
    const i18n = createInstance()
    await i18n.init({ lng: 'en', resources: {} })
    await act(async () =>
      root.render(
        <QueryClientProvider client={queryClient}>
          <I18nextProvider i18n={i18n}>
            <BindingDetailsDialog open onOpenChange={(v) => changes.push(v)} />
          </I18nextProvider>
        </QueryClientProvider>
      )
    )
    const close = document.body.querySelector(
      '[data-slot="dialog-close"]'
    ) as HTMLButtonElement
    assert.ok(close)
    await act(async () => close.click())
    assert.deepEqual(changes, [false])
    assert.equal(calls, 1)
    await act(async () => root.unmount())
    host.remove()
    api.defaults.adapter = original
  }
)
