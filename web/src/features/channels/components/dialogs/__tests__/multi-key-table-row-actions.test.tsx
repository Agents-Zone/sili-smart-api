import assert from 'node:assert/strict'
import { after, afterEach, describe, test } from 'node:test'

import { Window } from 'happy-dom'

const domWindow = new Window()
const domGlobals = [
  'window',
  'document',
  'navigator',
  'HTMLElement',
  'HTMLButtonElement',
  'SVGElement',
  'Node',
  'Element',
  'Event',
  'MouseEvent',
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
const { MultiKeyTableRowActions } = await import('../multi-key-table-row-actions')

const i18n = createInstance()
await i18n.use(initReactI18next).init({
  lng: 'en',
  resources: { en: { translation: {} } },
})

const reactTestGlobals = globalThis as typeof globalThis & {
  IS_REACT_ACT_ENVIRONMENT?: boolean
}
reactTestGlobals.IS_REACT_ACT_ENVIRONMENT = true

type RenderedRow = {
  host: HTMLDivElement
  root: ReturnType<typeof createRoot>
}

let renderedRow: RenderedRow | null = null

async function renderRowActions(
  options: {
    keyIndex?: number
    testingKeyIndex?: number | null
    canTest?: boolean
    onTest?: (keyIndex: number) => void
  } = {}
): Promise<RenderedRow> {
  const host = document.createElement('div')
  document.body.append(host)
  const root = createRoot(host)
  renderedRow = { host, root }

  await act(async () =>
    root.render(
      <I18nextProvider i18n={i18n}>
        <MultiKeyTableRowActions
          keyIndex={options.keyIndex ?? 0}
          status={1}
          canTest={options.canTest ?? true}
          testingKeyIndex={options.testingKeyIndex ?? null}
          canDelete
          onTest={options.onTest ?? (() => {})}
          onAction={() => {}}
        />
      </I18nextProvider>
    )
  )
  return renderedRow
}

function testButton(host: ParentNode): HTMLButtonElement {
  const button = host.querySelector<HTMLButtonElement>(
    'button[aria-label="Test Connection"]'
  )
  assert.ok(button, 'test connection button not rendered')
  return button
}

afterEach(async () => {
  if (renderedRow) {
    await act(async () => renderedRow?.root.unmount())
    renderedRow.host.remove()
    renderedRow = null
  }
  document.body.replaceChildren()
})

after(() => {
  domWindow.close()
})

describe('MultiKeyTableRowActions test button', () => {
  test('disables the button and explains why when the user lacks test permission', async () => {
    const { host } = await renderRowActions({ canTest: false })

    const button = testButton(host)
    assert.equal(button.disabled, true)
    assert.equal(
      button.getAttribute('title'),
      'No permission to perform this action'
    )
  })

  test('shows a spinner and blocks the button while the row key is being tested', async () => {
    const { host } = await renderRowActions({
      keyIndex: 2,
      testingKeyIndex: 2,
    })

    const button = testButton(host)
    assert.equal(button.disabled, true)
    assert.ok(button.querySelector('svg.animate-spin'), 'spinner not shown')
  })

  test('keeps other rows clickable and reports their key index while another key is being tested', async () => {
    const tested: number[] = []
    const { host } = await renderRowActions({
      keyIndex: 3,
      testingKeyIndex: 0,
      onTest: (keyIndex) => tested.push(keyIndex),
    })

    const button = testButton(host)
    assert.equal(button.disabled, false)
    assert.equal(button.querySelector('svg.animate-spin'), null)

    await act(async () => button.click())
    assert.deepEqual(tested, [3])
  })
})
