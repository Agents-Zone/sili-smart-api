import assert from 'node:assert/strict'
import { after, afterEach, describe, test } from 'node:test'

import type { QueryClient } from '@tanstack/react-query'
import type { Root } from 'react-dom/client'
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

const { act, useState } = await import('react')
const { createRoot } = await import('react-dom/client')
const { QueryClient: QueryClientCtor, QueryClientProvider } =
  await import('@tanstack/react-query')
const { createInstance } = await import('i18next')
const { I18nextProvider, initReactI18next } = await import('react-i18next')
const { mock } = await import('bun:test')
const { labelDayButton } = await import('react-day-picker')
const { enUS } = await import('react-day-picker/locale')
const dayjs = (await import('@/lib/dayjs')).default
const { getRollingDateRange } = await import('@/lib/time')
const { resolveDayWindow } = await import(
  '@/features/dashboard/lib/use-topn-time-range'
)
const realApi = await import('@/features/dashboard/api')
const { TokenCharts } = await import('../token-charts')

type TokenChartsFilters = Parameters<typeof TokenCharts>[0]['filters']

// Mock the network boundary only: the dashboard api module. The query stays
// pending so charts render the skeleton; assertions only need the requested
// window, never the payload.
const apiCalls: { start_timestamp: number; end_timestamp: number }[] = []
mock.module('@/features/dashboard/api', () => ({
  ...realApi,
  getTokenQuotaDataByTokens: (
    params: {
      start_timestamp: number
      end_timestamp: number
    }
  ) => {
    apiCalls.push({ ...params })
    return new Promise<{ success: boolean; data: [] }>(() => {})
  },
}))

const i18n = createInstance()
await i18n.use(initReactI18next).init({
  lng: 'en',
  resources: { en: { translation: {} } },
})

const reactTestGlobals = globalThis as typeof globalThis & {
  IS_REACT_ACT_ENVIRONMENT?: boolean
}
reactTestGlobals.IS_REACT_ACT_ENVIRONMENT = true

// Mirrors the dashboard parent: filters live in state above TokenCharts and
// every onFiltersChange re-renders with the next selection applied.
function FiltersHarness(props: {
  initial: TokenChartsFilters
  onFiltersChange?: (filters: TokenChartsFilters) => void
}) {
  const [filters, setFilters] = useState(props.initial)
  return (
    <TokenCharts
      filters={filters}
      onFiltersChange={(next) => {
        setFilters(next)
        props.onFiltersChange?.(next)
      }}
    />
  )
}

function baseFilters(
  overrides: Partial<TokenChartsFilters> = {}
): TokenChartsFilters {
  return { timeGranularity: 'hour', selectedRange: 7, topLimit: 10, ...overrides }
}

// The rolling window is derived from "now", so a render between two probes can
// only produce one of the two candidate windows.
function rollingWindowCandidates(
  days: number,
  probes: Date[]
): { start_timestamp: number; end_timestamp: number }[] {
  return probes.map((probe) => {
    const { start, end } = getRollingDateRange(days, probe)
    return {
      start_timestamp: Math.floor(start.getTime() / 1000),
      end_timestamp: Math.floor(end.getTime() / 1000),
    }
  })
}

function sameWindow(
  actual: { start_timestamp: number; end_timestamp: number } | undefined,
  expected: { start_timestamp: number; end_timestamp: number }
): boolean {
  return (
    actual?.start_timestamp === expected.start_timestamp &&
    actual?.end_timestamp === expected.end_timestamp
  )
}

type Rendered = { host: HTMLDivElement; root: Root }

let rendered: Rendered | null = null
let client: QueryClient | null = null

async function flushQueries(times = 3) {
  for (let i = 0; i < times; i++) {
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 0))
    })
  }
}

async function renderTokenCharts(
  initial: TokenChartsFilters,
  onFiltersChange?: (filters: TokenChartsFilters) => void
): Promise<Rendered> {
  const host = document.createElement('div')
  document.body.append(host)
  const root = createRoot(host)
  const queryClient = new QueryClientCtor({
    defaultOptions: { queries: { retry: false } },
  })
  client = queryClient
  rendered = { host, root }
  await act(async () =>
    root.render(
      <QueryClientProvider client={queryClient}>
        <I18nextProvider i18n={i18n}>
          <FiltersHarness initial={initial} onFiltersChange={onFiltersChange} />
        </I18nextProvider>
      </QueryClientProvider>
    )
  )
  await flushQueries()
  return rendered
}

function tabTrigger(host: ParentNode, label: string): HTMLButtonElement {
  const tab = [
    ...host.querySelectorAll<HTMLButtonElement>('[data-slot="tabs-trigger"]'),
  ].find((el) => el.textContent?.trim() === label)
  assert.ok(tab, `tabs trigger "${label}" not rendered`)
  return tab
}

function datePickerTrigger(
  host: ParentNode | null
): HTMLButtonElement | null {
  if (!host) return null
  return host.querySelector<HTMLButtonElement>(
    'button[data-slot="popover-trigger"]'
  )
}

function isTabDisabled(tab: HTMLElement): boolean {
  return (
    tab.hasAttribute('disabled') ||
    tab.getAttribute('aria-disabled') === 'true' ||
    tab.getAttribute('data-disabled') !== null
  )
}

afterEach(async () => {
  if (rendered) {
    await act(async () => rendered?.root.unmount())
    rendered.host.remove()
    rendered = null
  }
  client?.clear()
  client = null
  apiCalls.length = 0
  document.body.replaceChildren()
})

after(() => {
  domWindow.close()
})

describe('TokenCharts selected-date mode wiring', () => {
  test('absent selectedDate requests the rolling window and top limit changes stay request-free', async () => {
    const before = new Date()
    const { host } = await renderTokenCharts(baseFilters({ selectedRange: 7 }))
    const after = new Date()

    const candidates = rollingWindowCandidates(7, [before, after])
    assert.ok(
      candidates.some((expected) => sameWindow(apiCalls[0], expected)),
      `expected a rolling 7-day window, got ${JSON.stringify(apiCalls[0])}`
    )
    assert.equal(apiCalls.length, 1)

    await act(async () => tabTrigger(host, 'Top 20').click())
    await flushQueries()

    assert.equal(apiCalls.length, 1, 'top limit change must not refetch')
  })

  test('selected historical date requests its natural-day window under the date cache key', async () => {
    await renderTokenCharts(baseFilters({ selectedDate: '2026-11-03' }))

    const expected = resolveDayWindow('2026-11-03')
    assert.ok(expected, 'probe window must resolve for a past date')
    assert.ok(sameWindow(apiCalls[0], expected))

    const keys = (client?.getQueryCache().getAll() ?? []).map(
      (query) => query.queryKey
    )
    assert.ok(
      keys.some(
        (key) =>
          JSON.stringify(key) ===
          JSON.stringify(['dashboard', 'token-quota', 'day', '2026-11-03'])
      ),
      `date cache key missing, got ${JSON.stringify(keys)}`
    )
  })

  test('clicking a range preset clears the selected date', async () => {
    const changes: TokenChartsFilters[] = []
    const { host } = await renderTokenCharts(
      baseFilters({ selectedRange: 1, selectedDate: '2026-11-03' }),
      (next) => changes.push({ ...next })
    )

    await act(async () => tabTrigger(host, '7 Days').click())
    await flushQueries()

    assert.equal(changes.length, 1)
    assert.equal(changes[0].selectedRange, 7)
    assert.equal(changes[0].selectedDate, undefined)
  })

  test('clearing the date returns to the previous preset', async () => {
    const changes: TokenChartsFilters[] = []
    const { host } = await renderTokenCharts(
      baseFilters({ selectedRange: 14, selectedDate: '2026-11-03' }),
      (next) => changes.push({ ...next })
    )

    const clearButton = host.querySelector('[aria-label="Clear"]')
    assert.ok(clearButton, 'clear button not rendered')

    await act(async () =>
      clearButton.dispatchEvent(
        new MouseEvent('click', { bubbles: true, cancelable: true })
      )
    )
    await flushQueries()

    assert.equal(changes.length, 1)
    assert.equal(changes[0].selectedDate, undefined)
    assert.equal(changes[0].selectedRange, 14)
  })

  test('picking a historical date from the calendar enters day mode and requests that window', async () => {
    const changes: TokenChartsFilters[] = []
    // Navigate one month back, then pick the 15th: always a past date and
    // always inside the displayed month grid. Set day 15 before subtracting
    // the month so dayjs month-end overflow cannot shift the target.
    const pickedDate = dayjs().date(15).subtract(1, 'month')
    const pickedDay = pickedDate.format('YYYY-MM-DD')
    const before = new Date()
    const { host } = await renderTokenCharts(baseFilters(), (next) =>
      changes.push({ ...next })
    )
    const after = new Date()

    assert.ok(
      rollingWindowCandidates(7, [before, after]).some((expected) =>
        sameWindow(apiCalls[0], expected)
      ),
      'rolling window expected before a date is picked'
    )

    const trigger = datePickerTrigger(host)
    assert.ok(trigger, 'date picker trigger not rendered')
    await act(async () => trigger.click())

    const previousButton = document.querySelector<HTMLButtonElement>(
      'button.rdp-button_previous'
    )
    assert.ok(previousButton, 'previous-month navigation button not rendered')
    await act(async () => previousButton.click())

    const dayButton = [
      ...document.querySelectorAll<HTMLButtonElement>('[role="grid"] button'),
    ].find(
      (el) =>
        el.getAttribute('aria-label') ===
        // Same label source as the rendered calendar (date-fns PPPP, en-US).
        labelDayButton(pickedDate.toDate(), {}, { locale: enUS })
    )
    assert.ok(dayButton, `${pickedDay} day button not rendered`)
    await act(async () => dayButton.click())
    await flushQueries()

    assert.equal(changes.length, 1)
    assert.equal(changes[0].selectedDate, pickedDay)

    const expected = resolveDayWindow(pickedDay)
    assert.ok(expected, 'probe window must resolve for a past date')
    const lastCall = apiCalls.at(-1)
    assert.ok(
      sameWindow(lastCall, expected),
      `day window expected after picking the date, got ${JSON.stringify(lastCall)}`
    )
  })

  test('malformed selected date falls back to the rolling window', async () => {
    const before = new Date()
    await renderTokenCharts(baseFilters({ selectedDate: 'not-a-date' }))
    const after = new Date()

    assert.ok(
      rollingWindowCandidates(7, [before, after]).some((expected) =>
        sameWindow(apiCalls[0], expected)
      ),
      `rolling window expected for a malformed date, got ${JSON.stringify(
        apiCalls[0]
      )}`
    )
  })

  test('top limit and granularity stay inert in data terms while a date is selected', async () => {
    const changes: TokenChartsFilters[] = []
    const { host } = await renderTokenCharts(
      baseFilters({ selectedDate: '2026-11-03' }),
      (next) => changes.push({ ...next })
    )

    assert.equal(apiCalls.length, 1)

    await act(async () => tabTrigger(host, 'Top 20').click())
    await flushQueries()
    assert.equal(apiCalls.length, 1, 'top limit change must not refetch')
    assert.equal(changes.length, 1)
    assert.equal(changes[0].topLimit, 20)
    assert.equal(changes[0].timeGranularity, 'hour')

    const granularity = tabTrigger(host, 'By Day')
    assert.equal(
      isTabDisabled(granularity),
      true,
      'granularity tabs must be disabled in day mode'
    )
    await act(async () => granularity.click())
    await flushQueries()
    assert.equal(changes.length, 1, 'disabled granularity must not fire')
  })
})
