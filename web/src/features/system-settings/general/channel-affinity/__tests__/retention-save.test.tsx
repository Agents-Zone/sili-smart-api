/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/
import assert from 'node:assert/strict'
import { after, test } from 'node:test'

import { Window } from 'happy-dom'

const dom = new Window()
const names = [
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
] as const
for (const key of names) {
  Object.defineProperty(globalThis, key, {
    configurable: true,
    value: dom[key],
  })
}
const reactGlobals = globalThis as typeof globalThis & {
  IS_REACT_ACT_ENVIRONMENT?: boolean
}
reactGlobals.IS_REACT_ACT_ENVIRONMENT = true
const { act } = await import('react')
const { createRoot } = await import('react-dom/client')
const { QueryClient, QueryClientProvider } =
  await import('@tanstack/react-query')
const { createInstance } = await import('i18next')
const { I18nextProvider } = await import('react-i18next')
const { api } = await import('@/lib/api')
const { ChannelAffinitySection } = await import('../index')
const { SettingsPageProvider } =
  await import('../../../components/settings-page-context')
after(async () => {
  await dom.happyDOM.close()
})

test('修改最近绑定保留期后保存独立配置，越界输入阻止提交', async () => {
  const i18n = createInstance()
  await i18n.init({ lng: 'en', resources: {} })
  const client = new QueryClient()
  const host = document.createElement('div')
  const actions = document.createElement('div')
  document.body.append(host, actions)
  const root = createRoot(host)
  const originalAdapter = api.defaults.adapter
  const updates: unknown[] = []
  api.defaults.adapter = async (config) => {
    if (config.method === 'put') updates.push(JSON.parse(config.data as string))
    return {
      data: { success: true },
      status: 200,
      statusText: 'OK',
      headers: {},
      config,
    }
  }
  try {
    await act(async () =>
      root.render(
        <QueryClientProvider client={client}>
          <I18nextProvider i18n={i18n}>
            <SettingsPageProvider actionsContainer={actions}>
              <ChannelAffinitySection
                defaultValues={{
                  'channel_affinity_setting.enabled': true,
                  'channel_affinity_setting.switch_on_success': true,
                  'channel_affinity_setting.keep_on_channel_disabled': false,
                  'channel_affinity_setting.max_entries': 1000,
                  'channel_affinity_setting.default_ttl_seconds': 60,
                  'channel_affinity_setting.last_bind_ttl_seconds': 604800,
                  'channel_affinity_setting.rules': '[]',
                }}
              />
            </SettingsPageProvider>
          </I18nextProvider>
        </QueryClientProvider>
      )
    )
    const label = [...document.querySelectorAll('label')].find(
      (node) => node.textContent === 'Previous binding retention (seconds)'
    )
    assert.ok(label)
    const input = document.querySelector<HTMLInputElement>(`#${label.htmlFor}`)
    assert.ok(input)
    const setter = Object.getOwnPropertyDescriptor(
      dom.HTMLInputElement.prototype,
      'value'
    )?.set
    assert.ok(setter)
    const save = [...actions.querySelectorAll('button')].find((node) =>
      node.textContent?.includes('Save')
    )
    assert.ok(save)
    await act(async () => {
      setter.call(input, '86400')
      input.dispatchEvent(
        new dom.Event('input', { bubbles: true }) as unknown as Event
      )
    })
    await act(async () => save.click())
    assert.deepEqual(updates, [
      { key: 'channel_affinity_setting.last_bind_ttl_seconds', value: '86400' },
    ])
    await act(async () => {
      setter.call(input, '0')
      input.dispatchEvent(
        new dom.Event('input', { bubbles: true }) as unknown as Event
      )
    })
    await act(async () => save.click())
    assert.equal(updates.length, 1, '越界值应在保存前被拦截')
  } finally {
    await act(async () => root.unmount())
    api.defaults.adapter = originalAdapter
    client.clear()
    host.remove()
    actions.remove()
  }
})
