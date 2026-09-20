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
import { after, describe, test } from 'node:test'

import { Window } from 'happy-dom'

import type { AffinityRule } from '../types'

const domWindow = new Window()
const domGlobals = [
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

for (const key of domGlobals) {
  Object.defineProperty(globalThis, key, {
    configurable: true,
    value: domWindow[key],
  })
}

const { act } = await import('react')
const { createRoot } = await import('react-dom/client')
const { createInstance } = await import('i18next')
const { I18nextProvider, initReactI18next } = await import('react-i18next')
const { Toaster } = await import('sonner')
const { RuleEditorDialog } = await import('../rule-editor-dialog')
const { RULE_TEMPLATES } = await import('../constants')

const EXCLUSIVE_BIND_LABEL = 'Exclusive Bind'
const EXCLUSIVE_BIND_WARNING_KEY =
  "Exclusive bind granularity follows this rule's affinity key. With token_id it equals per api_key exclusivity; with other key types one api_key may occupy multiple or all channels. When all available channels are occupied, reuse applies and exclusivity is suspended"

const i18n = createInstance()
await i18n.use(initReactI18next).init({
  lng: 'en',
  resources: {
    en: {
      translation: {
        'Exclusive Bind': EXCLUSIVE_BIND_LABEL,
        'When enabled, a channel is exclusively bound to a single affinity key; when all available channels are occupied, the channel with the fewest bindings is reused':
          'When enabled, a channel is exclusively bound to a single affinity key; when all available channels are occupied, the channel with the fewest bindings is reused',
        [EXCLUSIVE_BIND_WARNING_KEY]: EXCLUSIVE_BIND_WARNING_KEY,
      },
    },
  },
})

const reactTestGlobals = globalThis as typeof globalThis & {
  IS_REACT_ACT_ENVIRONMENT?: boolean
}
reactTestGlobals.IS_REACT_ACT_ENVIRONMENT = true

function buildRule(overrides: Record<string, unknown> = {}): AffinityRule {
  return {
    id: 1,
    name: 'codex cli trace',
    model_regex: ['^gpt-.*$'],
    path_regex: ['/v1/responses'],
    key_sources: [{ type: 'gjson', path: 'prompt_cache_key' }],
    value_regex: '',
    ttl_seconds: 0,
    skip_retry_on_failure: true,
    include_using_group: true,
    include_model_name: false,
    include_rule_name: true,
    ...overrides,
  }
}

function queryWarningText(): string | null {
  const text = document.body.textContent ?? ''
  return text.includes(EXCLUSIVE_BIND_WARNING_KEY) ? EXCLUSIVE_BIND_WARNING_KEY : null
}

function getExclusiveBindSwitch(): HTMLElement {
  const labels = [...document.querySelectorAll<HTMLLabelElement>('label')]
  const label = labels.find(
    (candidate) => candidate.textContent?.trim() === EXCLUSIVE_BIND_LABEL
  )
  assert.ok(label, `Expected label "${EXCLUSIVE_BIND_LABEL}"`)
  const row = label.closest('[data-settings-form-span="full"]')
  assert.ok(row, 'Expected the switch row container')
  const control = row.querySelector<HTMLElement>('[data-slot="switch"]')
  assert.ok(control, 'Expected a switch control next to the label')
  return control
}

async function renderDialog(props: {
  rule: ReturnType<typeof buildRule> | null
  onSave?: (rule: unknown) => void
}) {
  const savedRules: unknown[] = []
  const host = document.createElement('div')
  document.body.append(host)
  const root = createRoot(host)
  await act(async () =>
    root.render(
      <I18nextProvider i18n={i18n}>
        <RuleEditorDialog
          open
          onOpenChange={() => undefined}
          rule={props.rule}
          onSave={(rule) => {
            savedRules.push(rule)
            props.onSave?.(rule)
          }}
        />
        <Toaster duration={60_000} />
      </I18nextProvider>
    )
  )
  return {
    savedRules,
    cleanup: async () => {
      await act(async () => root.unmount())
      host.remove()
    },
  }
}

async function submitRuleForm() {
  const form = document.querySelector<HTMLFormElement>(
    '#channel-affinity-rule-form'
  )
  assert.ok(form)
  await act(async () =>
    form.dispatchEvent(
      new domWindow.Event('submit', {
        bubbles: true,
        cancelable: true,
      }) as unknown as Event
    )
  )
}

async function clickExclusiveBindSwitch() {
  await act(async () =>
    getExclusiveBindSwitch().dispatchEvent(
      new domWindow.Event('click', { bubbles: true }) as unknown as Event
    )
  )
}

describe('rule editor dialog exclusive bind switch', () => {
  after(() => {
    domWindow.close()
  })

  test('shows the exclusive-bind highlight warning when editing a rule with exclusive_bind true', async () => {
    const rendered = await renderDialog({
      rule: buildRule({ exclusive_bind: true }),
    })

    assert.ok(
      (document.body.textContent ?? '').includes(EXCLUSIVE_BIND_LABEL),
      'expected the Exclusive Bind switch label to render'
    )
    assert.equal(
      document.querySelector('[data-slot="alert"]')?.textContent ?? '',
      EXCLUSIVE_BIND_WARNING_KEY,
      'expected the highlight warning alert content to match the i18n key'
    )
    assert.notEqual(queryWarningText(), null)

    await rendered.cleanup()
  })

  test('hides the warning when the rule omits exclusive_bind', async () => {
    const rendered = await renderDialog({ rule: buildRule() })

    assert.equal(queryWarningText(), null)
    assert.equal(document.querySelector('[data-slot="alert"]'), null)

    await rendered.cleanup()
  })

  test('hides the warning when the rule sets exclusive_bind false', async () => {
    const rendered = await renderDialog({
      rule: buildRule({ exclusive_bind: false }),
    })

    assert.equal(queryWarningText(), null)

    await rendered.cleanup()
  })

  test('toggles the warning visibility in real time when the switch is clicked', async () => {
    const rendered = await renderDialog({
      rule: buildRule({ exclusive_bind: false }),
    })

    assert.equal(queryWarningText(), null)

    await clickExclusiveBindSwitch()
    assert.notEqual(
      queryWarningText(),
      null,
      'expected the warning to appear after enabling the switch'
    )

    await clickExclusiveBindSwitch()
    assert.equal(
      queryWarningText(),
      null,
      'expected the warning to disappear after disabling the switch'
    )

    await rendered.cleanup()
  })

  test('defaults exclusive_bind to false for a new rule', async () => {
    const rendered = await renderDialog({ rule: null })

    assert.ok(
      (document.body.textContent ?? '').includes(EXCLUSIVE_BIND_LABEL),
      'expected the Exclusive Bind switch label to render for a new rule'
    )
    assert.equal(queryWarningText(), null)

    await rendered.cleanup()
  })

  test('submits exclusive_bind with the saved rule payload', async () => {
    const rendered = await renderDialog({
      rule: buildRule({ exclusive_bind: true }),
    })
    await submitRuleForm()

    assert.equal(rendered.savedRules.length, 1)
    assert.equal(
      (rendered.savedRules[0] as { exclusive_bind?: boolean })
        .exclusive_bind,
      true
    )

    await rendered.cleanup()
  })

  test('submits exclusive_bind false after toggling the switch off', async () => {
    const rendered = await renderDialog({
      rule: buildRule({ exclusive_bind: true }),
    })
    await clickExclusiveBindSwitch()
    await submitRuleForm()

    assert.equal(rendered.savedRules.length, 1)
    assert.equal(
      (rendered.savedRules[0] as { exclusive_bind?: boolean })
        .exclusive_bind,
      false
    )

    await rendered.cleanup()
  })

  test('rule templates carry exclusive_bind false by default', () => {
    for (const key of Object.keys(RULE_TEMPLATES)) {
      assert.equal(RULE_TEMPLATES[key].exclusive_bind, false)
    }
  })
})
