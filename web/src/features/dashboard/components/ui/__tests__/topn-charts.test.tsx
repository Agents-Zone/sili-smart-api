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
  'HTMLSpanElement',
  'HTMLInputElement',
  'SVGElement',
  'Node',
  'Element',
  'Event',
  'MouseEvent',
  'KeyboardEvent',
  'FocusEvent',
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
const { Coins } = await import('lucide-react')
const { labelDayButton } = await import('react-day-picker')
const { enUS } = await import('react-day-picker/locale')
const { DatePicker } = await import('@/components/date-picker')
const dayjs = (await import('@/lib/dayjs')).default
const { TopNChartsSection } = await import('../topn-charts')

const i18n = createInstance()
await i18n.use(initReactI18next).init({
  lng: 'en',
  resources: { en: { translation: {} } },
})

const reactTestGlobals = globalThis as typeof globalThis & {
  IS_REACT_ACT_ENVIRONMENT?: boolean
}
reactTestGlobals.IS_REACT_ACT_ENVIRONMENT = true

type SectionProps = Parameters<typeof TopNChartsSection>[0]

function baseProps(): SectionProps {
  return {
    icon: Coins,
    charts: [],
    chartKeyPrefix: 'test',
    isLoading: true,
    selectedRange: 1,
    timeGranularity: 'hour',
    topLimit: 10,
    topLimitLabelKey: 'Top',
    onRangeChange: () => {},
    onGranularityChange: () => {},
    onTopLimitChange: () => {},
  }
}

type Rendered = {
  host: HTMLDivElement
  root: ReturnType<typeof createRoot>
}

let rendered: Rendered | null = null

async function renderSection(
  props: Partial<SectionProps> = {}
): Promise<Rendered> {
  const host = document.createElement('div')
  document.body.append(host)
  const root = createRoot(host)
  rendered = { host, root }
  await act(async () =>
    root.render(
      <I18nextProvider i18n={i18n}>
        <TopNChartsSection {...baseProps()} {...props} />
      </I18nextProvider>
    )
  )
  return rendered
}

async function renderDatePicker(
  props: Partial<Parameters<typeof DatePicker>[0]> = {}
): Promise<Rendered> {
  const host = document.createElement('div')
  document.body.append(host)
  const root = createRoot(host)
  rendered = { host, root }
  await act(async () =>
    root.render(
      <I18nextProvider i18n={i18n}>
        <DatePicker
          selected={undefined}
          onSelect={() => {}}
          {...props}
        />
      </I18nextProvider>
    )
  )
  return rendered
}

function tabTrigger(host: ParentNode, label: string): HTMLButtonElement {
  const tab = [...host.querySelectorAll<HTMLButtonElement>('[data-slot="tabs-trigger"]')].find(
    (el) => el.textContent?.trim() === label
  )
  assert.ok(tab, `tabs trigger "${label}" not rendered`)
  return tab
}

function isTabActive(tab: HTMLElement): boolean {
  return (
    tab.getAttribute('data-active') !== null ||
    tab.getAttribute('aria-selected') === 'true'
  )
}

function isTabDisabled(tab: HTMLElement): boolean {
  return (
    tab.hasAttribute('disabled') ||
    tab.getAttribute('aria-disabled') === 'true' ||
    tab.getAttribute('data-disabled') !== null
  )
}

function datePickerTrigger(
  host: ParentNode | null
): HTMLButtonElement | null {
  if (!host) return null
  return host.querySelector<HTMLButtonElement>(
    'button[data-slot="popover-trigger"]'
  )
}

afterEach(async () => {
  if (rendered) {
    await act(async () => rendered?.root.unmount())
    rendered.host.remove()
    rendered = null
  }
  document.body.replaceChildren()
})

after(() => {
  domWindow.close()
})

describe('TopNChartsSection date filter', () => {
  test('users shape without date callbacks renders no date picker and keeps tabs untouched', async () => {
    const { host } = await renderSection()

    assert.equal(datePickerTrigger(host), null)
    assert.equal(host.textContent?.includes('Pick a date'), false)
    assert.equal(isTabDisabled(tabTrigger(host, 'By Hour')), false)
    assert.equal(isTabActive(tabTrigger(host, '1 Day')), true)
    assert.equal(host.querySelector('[data-slot="tooltip-trigger"]'), null)
  })

  test('selected date shows its label, disables granularity tabs and clears preset highlight', async () => {
    const { host } = await renderSection({
      selectedDate: '2026-11-03',
      onSelectedDateChange: () => {},
      granularityDisabled: true,
      granularityDisabledTitleKey: 'View by hour on a specified date',
    })

    const trigger = datePickerTrigger(host)
    assert.ok(trigger, 'date picker trigger not rendered')
    assert.ok(trigger.textContent?.includes('2026-11-03'))

    // The date picker sits between the range presets and the granularity tabs.
    const FOLLOWING = Node.DOCUMENT_POSITION_FOLLOWING
    const lastPreset = tabTrigger(host, '29 Days')
    const firstGranularity = tabTrigger(host, 'By Hour')
    assert.ok(
      lastPreset.compareDocumentPosition(trigger) & FOLLOWING,
      'date picker should follow the range presets'
    )
    assert.ok(
      trigger.compareDocumentPosition(firstGranularity) & FOLLOWING,
      'date picker should precede the granularity tabs'
    )

    for (const label of ['By Hour', 'By Day', 'By Week']) {
      assert.equal(isTabDisabled(tabTrigger(host, label)), true, label)
    }
    for (const label of ['1 Day', '7 Days', '14 Days', '29 Days']) {
      assert.equal(isTabActive(tabTrigger(host, label)), false, label)
    }

    const tooltipTrigger = host.querySelector('[data-slot="tooltip-trigger"]')
    assert.ok(tooltipTrigger, 'granularity tooltip trigger not rendered')
    assert.ok(
      tooltipTrigger.querySelector('[data-slot="tabs-trigger"]'),
      'granularity tabs not wrapped by tooltip trigger'
    )
  })

  test('clear button reports undefined and keeps the calendar closed', async () => {
    const calls: (string | undefined)[] = []
    const { host } = await renderSection({
      selectedDate: '2026-11-03',
      onSelectedDateChange: (date) => calls.push(date),
    })

    const clearButton = host.querySelector('[aria-label="Clear"]')
    assert.ok(clearButton, 'clear button not rendered')

    await act(async () => clearButton.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true })))

    assert.deepEqual(calls, [undefined])
    assert.equal(document.querySelector('[role="grid"]'), null)
  })

  test('empty selected date keeps placeholder, granularity enabled and preset highlighted', async () => {
    const { host } = await renderSection({
      selectedDate: undefined,
      onSelectedDateChange: () => {},
    })

    const trigger = datePickerTrigger(host)
    assert.ok(trigger, 'date picker trigger not rendered')
    assert.ok(trigger.textContent?.includes('Pick a date'))
    assert.equal(isTabDisabled(tabTrigger(host, 'By Hour')), false)
    assert.equal(isTabActive(tabTrigger(host, '1 Day')), true)
  })

  test('selecting today from the calendar reports a yyyy-MM-dd string', async () => {
    const calls: (string | undefined)[] = []
    const { host } = await renderSection({
      onSelectedDateChange: (date) => calls.push(date),
    })

    await act(async () => datePickerTrigger(host)?.click())

    const todayButton = [
      ...document.querySelectorAll<HTMLButtonElement>('[role="grid"] button'),
    ].find((el) => (el.getAttribute('aria-label') ?? '').startsWith('Today,'))
    assert.ok(todayButton, 'today day button not rendered')

    await act(async () => todayButton.click())

    assert.equal(calls.length, 1)
    assert.equal(calls[0], dayjs().format('YYYY-MM-DD'))
  })

  test('future days in the calendar are disabled and report nothing', async () => {
    const calls: (string | undefined)[] = []
    const { host } = await renderSection({
      onSelectedDateChange: (date) => calls.push(date),
    })

    await act(async () => datePickerTrigger(host)?.click())

    // Tomorrow is always in the future. showOutsideDays keeps its button in
    // the grid even when it falls in the next month. The label uses the same
    // source as the rendered calendar (date-fns PPPP, en-US).
    const tomorrowLabel = labelDayButton(
      dayjs().add(1, 'day').toDate(),
      {},
      { locale: enUS }
    )
    const futureButton = [
      ...document.querySelectorAll<HTMLButtonElement>('[role="grid"] button'),
    ].find((el) => el.getAttribute('aria-label') === tomorrowLabel)
    assert.ok(futureButton, 'future day button not rendered')
    assert.equal(futureButton.disabled, true)

    await act(async () => futureButton.click())
    assert.deepEqual(calls, [])
  })
})

describe('DatePicker optional props', () => {
  test('keeps the calendar icon trigger when onClear and className are absent', async () => {
    const { host } = await renderDatePicker()

    const trigger = datePickerTrigger(host)
    assert.ok(trigger, 'date picker trigger not rendered')
    assert.equal(trigger.querySelector('[aria-label="Clear"]'), null)
    assert.ok(trigger.querySelector('svg'), 'calendar icon not rendered')
  })
})
