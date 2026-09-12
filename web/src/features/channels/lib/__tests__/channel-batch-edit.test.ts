import assert from 'node:assert/strict'
import { describe, test } from 'node:test'

import type { BatchUpdateParams } from '../../types'
import {
  buildBatchEditPayload,
  MAX_BATCH_EDIT_CHANNELS,
  type ChannelBatchEditForm,
} from '../channel-batch-edit'

const filledForm: ChannelBatchEditForm = {
  group: 'default,vip',
  tag: 'production',
  remark: 'temporarily downgraded',
  models: 'gpt-4o,claude-3-5-sonnet',
  model_mapping: '{"gpt-4o":"gpt-4o-mini"}',
  weight: '10',
  priority: '5',
  test_model: 'gpt-4o',
  auto_ban: 'enabled',
}

const expectedFullPayload: BatchUpdateParams = {
  ids: [7],
  group: 'default,vip',
  tag: 'production',
  remark: 'temporarily downgraded',
  models: 'gpt-4o,claude-3-5-sonnet',
  model_mapping: '{"gpt-4o":"gpt-4o-mini"}',
  weight: 10,
  priority: 5,
  test_model: 'gpt-4o',
  auto_ban: 1,
}

describe('buildBatchEditPayload', () => {
  test('skips empty fields', () => {
    const result = buildBatchEditPayload([1], {
      group: '  ',
      tag: '  ',
      remark: '   ',
    })

    assert.equal(result.payload, null)
    assert.equal(result.error, 'You must fill in at least one field')
    assert.deepEqual(result.fieldLabels, [])
  })

  test('keeps zero values', () => {
    const result = buildBatchEditPayload([1], {
      weight: '0',
      priority: '0',
      auto_ban: 'disabled',
    })

    assert.deepEqual(result.payload, {
      ids: [1],
      weight: 0,
      priority: 0,
      auto_ban: 0,
    })
    assert.equal(result.fieldLabels.length, 3)
    assert.equal(result.error, null)
  })

  test('rejects out-of-range values', () => {
    const cases: Array<{ form: ChannelBatchEditForm; error: string }> = [
      {
        form: { weight: '-1' },
        error: 'Weight must be between 0 and 4294967295',
      },
      {
        form: { weight: '4294967296' },
        error: 'Weight must be between 0 and 4294967295',
      },
      {
        form: { weight: '1.5' },
        error: 'Weight must be between 0 and 4294967295',
      },
      {
        form: { group: 'a'.repeat(65) },
        error: 'Group must be less than 64 characters',
      },
      { form: { group: 'a,,b' }, error: 'Group format error' },
      {
        form: { tag: 'a'.repeat(192) },
        error: 'Tag must be less than 191 characters',
      },
      {
        form: { remark: 'a'.repeat(256) },
        error: 'Remark must be less than 255 characters',
      },
      {
        form: { test_model: 'a'.repeat(256) },
        error: 'Test model must be less than 255 characters',
      },
      {
        form: { models: 'a'.repeat(256) },
        error: 'Model name must be less than 255 characters',
      },
      { form: { models: 'a,,b' }, error: 'Model list format error' },
      {
        form: { model_mapping: '[1,2]' },
        error: 'Model mapping must be a valid JSON object',
      },
      {
        form: { model_mapping: '  ' },
        error:
          'Model mapping cannot be empty here. Clear it in single-channel edit instead.',
      },
    ]

    for (const { form, error } of cases) {
      const result = buildBatchEditPayload([1], form)

      assert.equal(result.payload, null, JSON.stringify(form))
      assert.equal(result.error, error, JSON.stringify(form))
    }
  })

  test('rejects over-limit selection', () => {
    const ids = Array.from({ length: 201 }, (_, i) => i + 1)

    const result = buildBatchEditPayload(ids, { weight: '1' })

    assert.equal(result.payload, null)
    assert.equal(
      result.error,
      'The number of selected channels exceeds the limit of 200. Please split the operation.'
    )
  })
})

describe('buildBatchEditPayload boundaries', () => {
  test('rejects an empty selection', () => {
    const result = buildBatchEditPayload([], { weight: '1' })

    assert.equal(result.payload, null)
    assert.equal(result.error, 'No channels selected')
  })

  test('accepts exactly the channel limit', () => {
    const ids = Array.from({ length: MAX_BATCH_EDIT_CHANNELS }, (_, i) => i + 1)

    const result = buildBatchEditPayload(ids, { weight: '1' })

    assert.equal(result.error, null)
    assert.deepEqual(result.payload, { ids, weight: 1 })
    assert.deepEqual(result.fieldLabels, ['Weight'])
  })

  test('builds every filled field and labels them', () => {
    const result = buildBatchEditPayload([7], filledForm)

    assert.equal(result.error, null)
    assert.deepEqual(result.payload, expectedFullPayload)
    assert.deepEqual(result.fieldLabels, [
      'Group',
      'Tag',
      'Remark',
      'Models',
      'Model Mapping',
      'Weight',
      'Priority',
      'Test Model',
      'Auto Ban',
    ])
  })

  test('skips the unchanged auto ban and an omitted model mapping', () => {
    const result = buildBatchEditPayload([3], {
      auto_ban: 'unchanged',
      model_mapping: undefined,
      tag: 'prod',
    })

    assert.equal(result.error, null)
    assert.deepEqual(result.payload, { ids: [3], tag: 'prod' })
  })

  test('rejects model mappings that are not plain objects', () => {
    for (const modelMapping of ['null', '123', '"gpt-4o"', 'not json', '{}[']) {
      const result = buildBatchEditPayload([1], {
        model_mapping: modelMapping,
      })

      assert.equal(
        result.error,
        'Model mapping must be a valid JSON object',
        modelMapping
      )
    }
  })

  test('trims surrounding whitespace before submitting', () => {
    const result = buildBatchEditPayload([1], {
      group: ' default , vip ',
      tag: '  prod  ',
      models: ' gpt-4o , claude ',
      weight: ' 7 ',
    })

    assert.equal(result.error, null)
    assert.deepEqual(result.payload, {
      ids: [1],
      group: 'default , vip',
      tag: 'prod',
      models: 'gpt-4o , claude',
      weight: 7,
    })
  })

  test('accepts the inclusive length and value boundaries', () => {
    const result = buildBatchEditPayload([1], {
      group: 'a'.repeat(64),
      tag: 'a'.repeat(191),
      remark: 'a'.repeat(255),
      test_model: 'a'.repeat(255),
      models: 'a'.repeat(255),
      weight: '4294967295',
      priority: '9007199254740991',
      model_mapping: '{}',
    })

    assert.equal(result.error, null)
    assert.equal(result.fieldLabels.length, 8)
  })

  test('rejects priorities that are not safe integers', () => {
    for (const priority of ['1.5', 'abc', '9007199254740992', '']) {
      const result = buildBatchEditPayload([1], {
        priority,
        tag: 'prod',
      })

      if (priority === '') {
        assert.equal(result.error, null)
        continue
      }

      assert.equal(result.error, 'Priority is out of range', priority)
    }
  })
})
