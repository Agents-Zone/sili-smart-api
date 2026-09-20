import assert from 'node:assert/strict'
import { after, test } from 'node:test'
import { Window } from 'happy-dom'

const dom = new Window()
for (const key of ['window','document','navigator','HTMLElement','HTMLButtonElement','SVGElement','Node','Element','Event','MutationObserver','ResizeObserver','requestAnimationFrame','cancelAnimationFrame','getComputedStyle'] as const) Object.defineProperty(globalThis,key,{configurable:true,value:dom[key]})
;(globalThis as typeof globalThis & {IS_REACT_ACT_ENVIRONMENT?:boolean}).IS_REACT_ACT_ENVIRONMENT = true
const { act } = await import('react')
const { createRoot } = await import('react-dom/client')
const { createInstance } = await import('i18next')
const { I18nextProvider } = await import('react-i18next')
const { api } = await import('@/lib/api')
const { BindingDetailsDialog } = await import('../binding-details-dialog')
after(() => dom.close())

async function render(open = true) {
  const host = document.createElement('div'); document.body.append(host)
  const root = createRoot(host)
  const i18n = createInstance(); await i18n.init({lng:'en', resources:{}})
  await act(async () => root.render(<I18nextProvider i18n={i18n}><BindingDetailsDialog open={open} onOpenChange={() => undefined} /></I18nextProvider>))
  return {host, root}
}

test('rendersLoadingThenAggregatedBindings', async () => {
  const original = api.defaults.adapter
  api.defaults.adapter = async (config) => ({data:{success:true,data:[{channel_id:2,channel_name:'备用',tokens:[{token_id:102,token_name:'备'},{token_id:101,token_name:'主'}]},{channel_id:1,channel_name:'主渠道',tokens:[{token_id:3,token_name:'令牌'}]}]},status:200,statusText:'OK',headers:{},config})
  const {host,root} = await render()
  assert.match(host.textContent ?? '', /Loading/)
  await act(async () => await Promise.resolve())
  assert.match(host.textContent ?? '', /主渠道/)
  assert.match(host.textContent ?? '', /备（102）/)
  await act(async () => root.unmount()); host.remove(); api.defaults.adapter = original
})

test('rendersEmptyStateWhenNoBindings', async () => {
  const original = api.defaults.adapter
  api.defaults.adapter = async (config) => ({data:{success:true,data:[]},status:200,statusText:'OK',headers:{},config})
  const {host,root} = await render(); await act(async () => await Promise.resolve())
  assert.match(host.textContent ?? '', /No bindings/)
  await act(async () => root.unmount()); host.remove(); api.defaults.adapter = original
})

test('rendersErrorStateWhenRequestFails', async () => {
  const original = api.defaults.adapter
  api.defaults.adapter = async () => { throw new Error('failed') }
  const {host,root} = await render(); await act(async () => await Promise.resolve())
  assert.match(host.textContent ?? '', /Failed to load bindings/)
  await act(async () => root.unmount()); host.remove(); api.defaults.adapter = original
})

test('closesDialogWithoutChangingCacheStats', async () => {
  let calls = 0
  const original = api.defaults.adapter
  api.defaults.adapter = async (config) => { calls++; return {data:{success:true,data:[]},status:200,statusText:'OK',headers:{},config} }
  const changes: boolean[] = []
  const host = document.createElement('div'); document.body.append(host); const root = createRoot(host); const i18n = createInstance(); await i18n.init({lng:'en',resources:{}})
  await act(async () => root.render(<I18nextProvider i18n={i18n}><BindingDetailsDialog open onOpenChange={(v) => changes.push(v)} /></I18nextProvider>))
  const close = host.querySelector('[data-slot="dialog-close"]') as HTMLButtonElement
  assert.ok(close); await act(async () => close.click())
  assert.deepEqual(changes,[false]); assert.equal(calls,1)
  await act(async () => root.unmount()); host.remove(); api.defaults.adapter = original
})
