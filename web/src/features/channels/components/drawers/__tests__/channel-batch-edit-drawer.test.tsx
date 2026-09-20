import assert from 'node:assert/strict'
import { after, afterEach, describe, test } from 'node:test'

import { Window } from 'happy-dom'

import type { BatchUpdateOutcome } from '../../../lib/channel-actions'
import type { BatchUpdateParams, BatchUpdateResult } from '../../../types'

const domWindow = new Window()
const domGlobals = [
  'window',
  'document',
  'navigator',
  'HTMLElement',
  'HTMLButtonElement',
  'HTMLInputElement',
  'HTMLFormElement',
  'SVGElement',
  'Node',
  'Element',
  'Event',
  'KeyboardEvent',
  'PointerEvent',
  'MouseEvent',
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
const { QueryClient, QueryClientProvider } =
  await import('@tanstack/react-query')
const { api } = await import('@/lib/api')
const { toast } = await import('sonner')
const { handleBatchUpdate } = await import('../../../lib/channel-actions')
const { ChannelBatchEditDrawer } = await import('../channel-batch-edit-drawer')

const toastErrorCalls: unknown[][] = []
const originalToastError = toast.error

/** Replace toast.error with a spy so tests can assert the user-visible error path. */
function spyOnToastError() {
  toastErrorCalls.length = 0
  toast.error = ((...args: unknown[]) => {
    toastErrorCalls.push(args)
  }) as typeof toast.error
}

const i18n = createInstance()
await i18n.use(initReactI18next).init({
  lng: 'en',
  resources: { en: { translation: {} } },
})

const reactTestGlobals = globalThis as typeof globalThis & {
  IS_REACT_ACT_ENVIRONMENT?: boolean
}
reactTestGlobals.IS_REACT_ACT_ENVIRONMENT = true

type ApiMethod = (url: string, data?: unknown) => Promise<{ data: unknown }>
type MockableApi = {
  get: ApiMethod
  post: ApiMethod
}
type RenderedDrawer = {
  host: HTMLDivElement
  root: ReturnType<typeof createRoot>
  queryClient: InstanceType<typeof QueryClient>
  openChanges: boolean[]
}

const apiClient = api as unknown as MockableApi
const originalGet = apiClient.get
const originalPost = apiClient.post
let renderedDrawer: RenderedDrawer | null = null

function installApiFixtures() {
  apiClient.get = async (url) => {
    switch (url) {
      case '/api/group/':
        return { data: { success: true, data: ['default', 'vip'] } }
      case '/api/channel/models':
        return { data: { success: true, data: [{ id: 'gpt-4o' }] } }
      default:
        throw new Error(`Unexpected GET ${url}`)
    }
  }
}

function installBatchUpdateResponse(response: BatchUpdateResult) {
  apiClient.post = (async (url: string, data?: unknown) => {
    assert.equal(url, '/api/channel/batch/update')
    assert.ok(data && typeof data === 'object')
    return { data: response }
  }) as unknown as typeof api.post
}

async function waitFor(
  condition: () => boolean,
  failureMessage: string
): Promise<void> {
  for (let attempt = 0; attempt < 50; attempt += 1) {
    if (condition()) return
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 10))
    })
  }
  assert.ok(condition(), failureMessage)
}

const sheetContent = () =>
  document.querySelector<HTMLElement>('[data-slot="sheet-content"]')
const dialogContent = () =>
  document.querySelector<HTMLElement>('[data-slot="dialog-content"]')

function findButton(container: ParentNode, text: string): HTMLButtonElement {
  const button = [
    ...container.querySelectorAll<HTMLButtonElement>('button'),
  ].find((candidate) => candidate.textContent?.includes(text))
  assert.ok(button, `Expected a button containing "${text}"`)
  return button
}

/**
 * Resolve a form control through its label: the RHF FormControl owns the generated
 * id, so tests look the input up by the label text the user actually sees.
 */
function inputByLabel(label: string): HTMLInputElement {
  const labelElement = [
    ...document.querySelectorAll<HTMLLabelElement>('label'),
  ].find((candidate) => candidate.textContent?.trim() === label)
  assert.ok(labelElement, `Expected a label "${label}"`)
  const id = labelElement.getAttribute('for')
  assert.ok(id, `Label "${label}" is not associated with a control`)
  const input = document.querySelector<HTMLInputElement>(`#${id}`)
  assert.ok(input, `Expected an input labelled "${label}"`)
  return input
}

async function click(button: HTMLButtonElement) {
  await act(async () => button.click())
}

async function changeInput(input: HTMLInputElement, value: string) {
  await act(async () => {
    const valueSetter = Object.getOwnPropertyDescriptor(
      domWindow.HTMLInputElement.prototype,
      'value'
    )?.set
    assert.ok(valueSetter)
    valueSetter.call(input, value)
    input.dispatchEvent(
      new domWindow.Event('input', { bubbles: true }) as unknown as Event
    )
  })
}

async function renderDrawer(
  options: {
    selectedIds?: number[]
    submit?: (payload: BatchUpdateParams) => Promise<BatchUpdateOutcome>
  } = {}
): Promise<RenderedDrawer> {
  const host = document.createElement('div')
  document.body.append(host)
  const root = createRoot(host)
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  const openChanges: boolean[] = []
  const rendered: RenderedDrawer = { host, root, queryClient, openChanges }
  renderedDrawer = rendered

  await act(async () =>
    root.render(
      <QueryClientProvider client={queryClient}>
        <I18nextProvider i18n={i18n}>
          <ChannelBatchEditDrawer
            open
            onOpenChange={(open) => openChanges.push(open)}
            selectedIds={options.selectedIds ?? [1, 2, 3]}
            submit={options.submit}
          />
        </I18nextProvider>
      </QueryClientProvider>
    )
  )
  await waitFor(() => sheetContent() !== null, 'batch edit drawer not rendered')
  return rendered
}

afterEach(async () => {
  apiClient.get = originalGet
  apiClient.post = originalPost
  toast.error = originalToastError
  toastErrorCalls.length = 0
  if (renderedDrawer) {
    await act(async () => renderedDrawer?.root.unmount())
    renderedDrawer.queryClient.clear()
    renderedDrawer.host.remove()
    renderedDrawer = null
  }
  document.body.replaceChildren()
})

after(() => {
  domWindow.close()
})

describe('ChannelBatchEditDrawer', () => {
  test('shows confirmation summary and keeps the drawer open after cancelling', async () => {
    installApiFixtures()
    await renderDrawer({ selectedIds: [1, 2, 3] })

    await changeInput(inputByLabel('Weight'), '10')
    await click(findButton(sheetContent() ?? document, 'Save'))

    await waitFor(
      () => dialogContent() !== null,
      'confirmation dialog did not open'
    )
    const dialog = dialogContent()
    assert.ok(dialog)
    assert.equal(dialog.textContent?.includes('3'), true)
    assert.equal(dialog.textContent?.includes('Weight'), true)

    await click(findButton(dialog, 'Cancel'))

    assert.equal(dialogContent(), null)
    assert.ok(sheetContent())
    assert.equal(inputByLabel('Weight').value, '10')
  })

  test('renders failure details, the rollback message and keeps the form for a retry', async () => {
    installApiFixtures()
    const submitted: BatchUpdateParams[] = []
    await renderDrawer({
      selectedIds: [1, 2, 3],
      submit: async (payload) => {
        submitted.push(payload)
        return {
          ok: false,
          count: 0,
          failed: [{ id: 2, reason: 'Failed to rebuild abilities' }],
          message: 'Batch edit failed, all changes were rolled back',
        }
      },
    })

    await changeInput(inputByLabel('Weight'), '10')
    await click(findButton(sheetContent() ?? document, 'Save'))
    await waitFor(
      () => dialogContent() !== null,
      'confirmation dialog did not open'
    )
    const dialog = dialogContent()
    assert.ok(dialog)
    await click(findButton(dialog, 'Confirm'))

    await waitFor(
      () =>
        document.body.textContent?.includes('Failed to rebuild abilities') ===
        true,
      'failure details were not rendered'
    )

    assert.deepEqual(submitted, [{ ids: [1, 2, 3], weight: 10 }])
    const sheet = sheetContent()
    assert.ok(sheet)
    assert.equal(sheet.textContent?.includes('Channel #2'), true)
    // 服务端本地化的回滚说明必须可见：用户需要知道没有任何渠道被修改
    assert.equal(
      sheet.textContent?.includes(
        'Batch edit failed, all changes were rolled back'
      ),
      true
    )
    const saveButton = findButton(sheet, 'Save')
    assert.equal(saveButton.disabled, false)
    assert.equal(saveButton.querySelector('.animate-spin'), null)
    assert.equal(inputByLabel('Weight').value, '10')
  })

  test('keeps the dialog closed and reports the issue under the field', async () => {
    installApiFixtures()
    await renderDrawer({ selectedIds: [1] })

    await changeInput(inputByLabel('Remark'), 'r'.repeat(256))
    await click(findButton(sheetContent() ?? document, 'Save'))

    assert.equal(dialogContent(), null)
    const sheet = sheetContent()
    assert.ok(sheet)
    assert.equal(
      sheet.textContent?.includes('Remark must be less than 255 characters'),
      true
    )
  })

  test('opens the confirmation dialog only after a field is filled', async () => {
    installApiFixtures()
    await renderDrawer({ selectedIds: [1, 2] })

    await click(findButton(sheetContent() ?? document, 'Save'))
    assert.equal(dialogContent(), null)

    await changeInput(inputByLabel('Remark'), 'batch')
    await click(findButton(sheetContent() ?? document, 'Save'))

    await waitFor(
      () => dialogContent() !== null,
      'confirmation dialog did not open'
    )
    assert.ok(sheetContent())
  })

  test('truncates a long field summary and keeps the full value in the title', async () => {
    installApiFixtures()
    await renderDrawer({ selectedIds: [4] })

    const longRemark = 'r'.repeat(60)
    await changeInput(inputByLabel('Remark'), longRemark)
    await click(findButton(sheetContent() ?? document, 'Save'))
    await waitFor(
      () => dialogContent() !== null,
      'confirmation dialog did not open'
    )

    const item = [...(dialogContent()?.querySelectorAll('li') ?? [])].find(
      (candidate) => candidate.getAttribute('title') === longRemark
    )
    assert.ok(item)
    assert.equal(item.textContent, `Remark: ${'r'.repeat(50)}...`)
  })

  test('closes the drawer after a successful submit', async () => {
    installApiFixtures()
    let committed = 0
    const submitted: BatchUpdateParams[] = []
    const rendered = await renderDrawer({
      selectedIds: [5, 6],
      submit: async (payload) => {
        submitted.push(payload)
        committed += 1
        return { ok: true, count: 2, failed: [] }
      },
    })

    await changeInput(inputByLabel('Remark'), 'batch')
    await click(findButton(sheetContent() ?? document, 'Save'))
    await waitFor(
      () => dialogContent() !== null,
      'confirmation dialog did not open'
    )
    const dialog = dialogContent()
    assert.ok(dialog)
    await click(findButton(dialog, 'Confirm'))

    await waitFor(
      () => rendered.openChanges.includes(false),
      'drawer was not closed after a successful submit'
    )
    assert.deepEqual(submitted, [{ ids: [5, 6], remark: 'batch' }])
    assert.equal(committed, 1)
  })
})

describe('handleBatchUpdate', () => {
  test('returns failure details for the drawer to render', async () => {
    installBatchUpdateResponse({
      success: false,
      message: '批量编辑失败，已全部回滚',
      data: { failed: [{ id: 2, reason: 'boom' }] },
    })

    const outcome = await handleBatchUpdate({ ids: [2, 3], tag: 'vip' })

    assert.deepEqual(outcome, {
      ok: false,
      count: 0,
      failed: [{ id: 2, reason: 'boom' }],
      message: '批量编辑失败，已全部回滚',
    })
  })

  test('renders no toast when failures carry per-channel details', async () => {
    spyOnToastError()
    installBatchUpdateResponse({
      success: false,
      message: '批量编辑失败，已全部回滚',
      data: { failed: [{ id: 2, reason: 'boom' }] },
    })

    await handleBatchUpdate({ ids: [2, 3], tag: 'vip' })

    assert.equal(toastErrorCalls.length, 0)
  })

  test('toasts the response message when failures carry no details', async () => {
    spyOnToastError()
    installBatchUpdateResponse({ success: false, message: '参数错误' })

    await handleBatchUpdate({ ids: [2, 3], tag: 'vip' })

    assert.deepEqual(toastErrorCalls, [['参数错误']])
  })

  test('refreshes the channel list and reports the updated count on success', async () => {
    installBatchUpdateResponse({ success: true, data: { count: 2 } })
    const queryClient = new QueryClient()
    const invalidated: unknown[] = []
    queryClient.invalidateQueries = async (filters) => {
      invalidated.push(filters)
    }
    const committed: number[] = []

    const outcome = await handleBatchUpdate(
      { ids: [1, 2], weight: 3 },
      queryClient,
      (count) => committed.push(count)
    )

    assert.deepEqual(outcome, { ok: true, count: 2, failed: [] })
    assert.deepEqual(committed, [2])
    assert.deepEqual(invalidated, [{ queryKey: ['channels', 'list'] }])
  })
})
