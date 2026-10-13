/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.
*/
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
  'HTMLInputElement',
  'HTMLFormElement',
  'HTMLLabelElement',
  'SVGElement',
  'Node',
  'Element',
  'Event',
  'CustomEvent',
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

async function render(open = true, onCacheCleared?: () => void) {
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
          <BindingDetailsDialog
            open={open}
            onOpenChange={() => undefined}
            onCacheCleared={onCacheCleared}
          />
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

async function waitFor(condition: () => boolean, what: string) {
  for (let i = 0; i < 100; i++) {
    if (condition()) return
    await act(async () => {
      await new Promise<void>((resolve) => setTimeout(resolve, 0))
    })
  }
  assert.fail(`Timed out waiting for ${what}`)
}

async function waitForText(text: string) {
  await waitFor(
    () => (document.body.textContent ?? '').includes(text),
    text
  )
}

function queryClearButtons(): HTMLElement[] {
  return [
    ...document.querySelectorAll<HTMLElement>(
      'button[title="Clear this binding"]'
    ),
  ]
}

function findButtonByText(text: string): HTMLButtonElement {
  const button = [
    ...document.querySelectorAll<HTMLButtonElement>('button'),
  ].find((node) => node.textContent?.trim().includes(text))
  assert.ok(button, `Expected button "${text}"`)
  return button
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
              {
                token_id: 102,
                token_name: '备',
                key_suffix: 'rule:102',
              },
              {
                token_id: 101,
                token_name: '主',
                key_suffix: 'rule:101',
              },
            ],
          },
          {
            channel_id: 1,
            channel_name: '主渠道',
            tokens: [
              { token_id: 3, token_name: '令牌', key_suffix: 'rule:3' },
            ],
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

const bindingRows = {
  both: [
    {
      channel_id: 1,
      channel_name: '主渠道',
      tokens: [
        {
          token_id: 101,
          token_name: '生产令牌',
          model_name: 'gpt-4',
          key_suffix: 'rule:gpt-4:101',
        },
        {
          token_id: 101,
          token_name: '生产令牌',
          model_name: 'gpt-3.5-turbo',
          key_suffix: 'rule:gpt-3.5-turbo:101',
        },
        {
          token_id: 102,
          token_name: '备用令牌',
          key_suffix: 'rule:gpt-4:102',
        },
      ],
    },
    {
      channel_id: 2,
      channel_name: '海外渠道',
      tokens: [
        {
          token_id: 103,
          token_name: '海外令牌',
          key_suffix: 'rule:gpt-4:103',
        },
      ],
    },
  ],
  remaining: [
    {
      channel_id: 1,
      channel_name: '主渠道',
      tokens: [
        {
          token_id: 101,
          token_name: '生产令牌',
          model_name: 'gpt-3.5-turbo',
          key_suffix: 'rule:gpt-3.5-turbo:101',
        },
        {
          token_id: 102,
          token_name: '备用令牌',
          key_suffix: 'rule:gpt-4:102',
        },
      ],
    },
    {
      channel_id: 2,
      channel_name: '海外渠道',
      tokens: [
        {
          token_id: 103,
          token_name: '海外令牌',
          key_suffix: 'rule:gpt-4:103',
        },
      ],
    },
  ],
}

test(
  'clearsSingleBindingAfterConfirmAndRefreshesList',
  { concurrency: false },
  async () => {
    const original = api.defaults.adapter
    let cleared = false
    const deleteCalls: unknown[] = []
    let cacheClearedCalls = 0
    api.defaults.adapter = async (config) => {
      const url = String(config.url ?? '')
      if (
        config.method === 'get' &&
        url.includes('channel_affinity_bindings')
      ) {
        return {
          data: {
            success: true,
            data: cleared ? bindingRows.remaining : bindingRows.both,
          },
          status: 200,
          statusText: 'OK',
          headers: {},
          config,
        }
      }
      if (
        config.method === 'delete' &&
        url.includes('channel_affinity_bindings')
      ) {
        deleteCalls.push(config.params)
        cleared = true
        return {
          data: { success: true, data: { deleted: 1 } },
          status: 200,
          statusText: 'OK',
          headers: {},
          config,
        }
      }
      return {
        data: { success: true },
        status: 200,
        statusText: 'OK',
        headers: {},
        config,
      }
    }
    const { host, root } = await render(true, () => {
      cacheClearedCalls++
    })

    await waitFor(() => queryClearButtons().length === 4, 'binding rows')
    assert.equal(
      queryClearButtons().length,
      4,
      '每条令牌绑定应渲染一个清除按钮'
    )

    await act(async () => queryClearButtons()[0].click())
    const confirmText = document.body.textContent ?? ''
    assert.ok(
      confirmText.includes('Confirm clearing this binding'),
      '点击清除按钮应弹出确认弹窗'
    )
    assert.ok(
      confirmText.includes('gpt-4'),
      '确认弹窗应展示目标绑定所属模型'
    )

    await act(async () => findButtonByText('Continue').click())
    assert.deepEqual(
      deleteCalls,
      [{ key_suffix: 'rule:gpt-4:101' }],
      '确认后应携带 key_suffix 调用单条清除接口'
    )
    await waitFor(() => queryClearButtons().length === 3, 'refreshed rows')
    assert.equal(cacheClearedCalls, 1, '清除成功后应通知父级刷新缓存统计')
    const listText = document.body.textContent ?? ''
    assert.equal(
      listText.includes('gpt-4'),
      false,
      '被清除的单条绑定（101@gpt-4）应从列表移除'
    )
    assert.equal(
      listText.includes('gpt-3.5-turbo'),
      true,
      '同令牌其他模型的绑定应保留'
    )
    assert.equal(
      listText.includes('备用令牌'),
      true,
      '同渠道其他令牌的绑定应保留'
    )

    await act(async () => root.unmount())
    host.remove()
    api.defaults.adapter = original
  }
)

test(
  'cancelingBindingClearSkipsDeleteRequest',
  { concurrency: false },
  async () => {
    const original = api.defaults.adapter
    const deleteCalls: unknown[] = []
    api.defaults.adapter = async (config) => {
      const url = String(config.url ?? '')
      if (
        config.method === 'delete' &&
        url.includes('channel_affinity_bindings')
      ) {
        deleteCalls.push(config.params)
      }
      return {
        data: {
          success: true,
          data:
            config.method === 'get' &&
            url.includes('channel_affinity_bindings')
              ? bindingRows.both
              : undefined,
        },
        status: 200,
        statusText: 'OK',
        headers: {},
        config,
      }
    }
    const { host, root } = await render()

    await waitFor(() => queryClearButtons().length === 4, 'binding rows')
    await act(async () => queryClearButtons()[0].click())
    await act(async () => findButtonByText('Cancel').click())

    assert.deepEqual(deleteCalls, [], '取消确认不应调用清除接口')
    assert.equal(
      (document.body.textContent ?? '').includes(
        'Confirm clearing this binding'
      ),
      false,
      '取消后确认弹窗应关闭'
    )
    assert.equal(
      queryClearButtons().length,
      4,
      '取消后绑定行应保持不变'
    )

    await act(async () => root.unmount())
    host.remove()
    api.defaults.adapter = original
  }
)
