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
import { test } from 'node:test'

import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { Window } from 'happy-dom'
import { createInstance } from 'i18next'
import { renderToStaticMarkup } from 'react-dom/server'
import { I18nextProvider } from 'react-i18next'

import { ChannelAffinitySection } from '../index'

test('最近绑定保留期显示独立配置并关联输入标签', async () => {
  const i18n = createInstance()
  await i18n.init({ lng: 'en', resources: {} })
  const client = new QueryClient()
  const html = renderToStaticMarkup(
    <QueryClientProvider client={client}>
      <I18nextProvider i18n={i18n}>
        <ChannelAffinitySection
          defaultValues={{
            'channel_affinity_setting.enabled': true,
            'channel_affinity_setting.switch_on_success': true,
            'channel_affinity_setting.keep_on_channel_disabled': false,
            'channel_affinity_setting.max_entries': 1000,
            'channel_affinity_setting.default_ttl_seconds': 60,
            'channel_affinity_setting.last_bind_ttl_seconds': 86400,
            'channel_affinity_setting.rules': '[]',
          }}
        />
      </I18nextProvider>
    </QueryClientProvider>
  )
  const window = new Window()
  try {
    window.document.body.innerHTML = html
    const label = [...window.document.querySelectorAll('label')].find(
      (element) =>
        element.textContent === 'Previous binding retention (seconds)'
    )
    assert.ok(label, '设置页应提供独立的最近绑定保留期')
    const input = window.document.querySelector(`#${label.htmlFor}`)
    assert.equal(input?.getAttribute('value'), '86400')
    assert.equal(input?.getAttribute('min'), '1')
    assert.equal(input?.getAttribute('max'), '31536000')
  } finally {
    client.clear()
    await window.happyDOM.close()
  }
})
