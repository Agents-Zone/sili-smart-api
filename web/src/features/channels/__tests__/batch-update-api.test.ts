import assert from 'node:assert/strict'
import { beforeEach, describe, test } from 'node:test'
import { mock } from 'bun:test'

import type { BatchUpdateParams, BatchUpdateResult } from '../types'

// bun:test 提供模块桩能力，node:test 在 bun 1.3.8 下未导出 mock.module
const postCalls: unknown[][] = []
let postResult: BatchUpdateResult = { success: true, data: { count: 2 } }

mock.module('@/lib/api', () => ({
  api: {
    post: async (...args: unknown[]) => {
      postCalls.push(args)
      return { data: postResult }
    },
  },
}))

const { batchUpdateChannels } = await import('../api')

describe('batchUpdateChannels', () => {
  beforeEach(() => {
    postCalls.length = 0
    postResult = { success: true, data: { count: 2 } }
  })

  test('batchUpdateChannels posts to the batch update endpoint', async () => {
    const payload: BatchUpdateParams = { ids: [1, 2], weight: 0 }

    const result = await batchUpdateChannels(payload)

    assert.equal(postCalls.length, 1)
    const [path, body, config] = postCalls[0] as [
      string,
      unknown,
      Record<string, unknown>,
    ]

    assert.equal(path, '/api/channel/batch/update')
    assert.deepEqual(body, { ids: [1, 2], weight: 0 })
    assert.equal(config.skipBusinessError, true)
    assert.equal(config.skipErrorHandler, true)
    assert.deepEqual(result, { success: true, data: { count: 2 } })
  })

  test('keeps an explicit auto_ban of 0 in the payload instead of skipping it', async () => {
    await batchUpdateChannels({ ids: [7], auto_ban: 0 })

    assert.deepEqual(postCalls[0]?.[1], { ids: [7], auto_ban: 0 })
  })

  test('unwraps a rollback failure response for the drawer to render', async () => {
    postResult = {
      success: false,
      message: '批量编辑失败，已全部回滚',
      data: { failed: [{ id: 2, reason: '更新 abilities 失败' }] },
    }

    const result = await batchUpdateChannels({ ids: [2, 3], tag: 'vip' })

    assert.deepEqual(result, {
      success: false,
      message: '批量编辑失败，已全部回滚',
      data: { failed: [{ id: 2, reason: '更新 abilities 失败' }] },
    })
  })
})
