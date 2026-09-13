import assert from 'node:assert/strict'
import { describe, test } from 'node:test'

import type { BatchUpdateParams } from '../../types'
import {
  buildBatchEditSubmission,
  channelBatchEditSchema,
  MAX_BATCH_EDIT_CHANNELS,
  validateBatchEditTargets,
  type ChannelBatchEditFormValues,
} from '../channel-batch-edit'

const filledForm: ChannelBatchEditFormValues = {
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

/** First schema issue message (an i18n key), undefined when the form is valid. */
const firstIssue = (values: ChannelBatchEditFormValues): string | undefined => {
  const result = channelBatchEditSchema.safeParse(values)
  return result.success ? undefined : result.error.issues[0]?.message
}

describe('channelBatchEditSchema', () => {
  test('accepts an empty form: every field is optional and blank means skip', () => {
    assert.equal(firstIssue({}), undefined)
    assert.equal(firstIssue({ group: '  ', tag: '   ', remark: '' }), undefined)
  })

  test('rejects out-of-range values', () => {
    const cases: Array<{ values: ChannelBatchEditFormValues; issue: string }> = [
      {
        values: { weight: '-1' },
        issue: 'Weight must be between 0 and 4294967295',
      },
      {
        values: { weight: '4294967296' },
        issue: 'Weight must be between 0 and 4294967295',
      },
      {
        values: { weight: '1.5' },
        issue: 'Weight must be between 0 and 4294967295',
      },
      {
        values: { group: 'a'.repeat(65) },
        issue: 'Group must be less than 64 characters',
      },
      { values: { group: 'a,,b' }, issue: 'Group format error' },
      {
        values: { tag: 'a'.repeat(192) },
        issue: 'Tag must be less than 191 characters',
      },
      {
        values: { remark: 'a'.repeat(256) },
        issue: 'Remark must be less than 255 characters',
      },
      {
        values: { test_model: 'a'.repeat(256) },
        issue: 'Test model must be less than 255 characters',
      },
      {
        values: { models: 'a'.repeat(256) },
        issue: 'Model name must be less than 255 characters',
      },
      { values: { models: 'a,,b' }, issue: 'Model list format error' },
      {
        values: { model_mapping: '[1,2]' },
        issue: 'Model mapping must be a valid JSON object',
      },
      {
        values: { model_mapping: '  ' },
        issue:
          'Model mapping cannot be empty here. Clear it in single-channel edit instead.',
      },
    ]

    for (const { values, issue } of cases) {
      assert.equal(firstIssue(values), issue, JSON.stringify(values))
    }
  })

  test('counts lengths in code points, matching the backend rune count', () => {
    // 255 个汉字合法：后端按码点计数，abilities.model 列宽 varchar(255) 按字符计
    assert.equal(firstIssue({ remark: '中'.repeat(255) }), undefined)
    assert.equal(
      firstIssue({ remark: '中'.repeat(256) }),
      'Remark must be less than 255 characters'
    )
    assert.equal(firstIssue({ group: '中'.repeat(64) }), undefined)
    assert.equal(firstIssue({ models: '中'.repeat(255) }), undefined)
  })

  test('rejects model mappings with non-string values', () => {
    // The relay path unmarshals the mapping into map[string]string; a numeric
    // value would break every request through the affected channels.
    assert.equal(
      firstIssue({ model_mapping: '{"gpt-4o":123}' }),
      'Model mapping values must be strings'
    )
  })

  test('rejects priorities beyond the safe integer range', () => {
    for (const priority of ['9007199254740992', '-9007199254740992']) {
      assert.equal(firstIssue({ priority }), 'Priority is out of range', priority)
    }
    assert.equal(firstIssue({ priority: '9007199254740991' }), undefined)
  })

  test('rejects model mappings that are not plain objects', () => {
    for (const model_mapping of ['null', '123', '"gpt-4o"', 'not json', '{}[']) {
      assert.equal(
        firstIssue({ model_mapping }),
        'Model mapping must be a valid JSON object',
        model_mapping
      )
    }
  })

  test('rejects priorities that are not safe integers', () => {
    for (const priority of ['1.5', 'abc']) {
      assert.equal(firstIssue({ priority }), 'Priority is out of range', priority)
    }
  })
})

describe('validateBatchEditTargets', () => {
  test('rejects an empty selection', () => {
    assert.equal(validateBatchEditTargets([]), 'No channels selected')
  })

  test('rejects over-limit selection', () => {
    const ids = Array.from({ length: 201 }, (_, i) => i + 1)

    assert.equal(
      validateBatchEditTargets(ids),
      'The number of selected channels exceeds the limit of 200. Please split the operation.'
    )
  })

  test('accepts exactly the channel limit', () => {
    const ids = Array.from({ length: MAX_BATCH_EDIT_CHANNELS }, (_, i) => i + 1)

    assert.equal(validateBatchEditTargets(ids), null)
  })
})

describe('buildBatchEditSubmission', () => {
  test('returns no payload when every field is blank', () => {
    const result = buildBatchEditSubmission([1], {
      group: '  ',
      tag: '  ',
      remark: '   ',
    })

    assert.equal(result.payload, null)
    assert.deepEqual(result.fields, [])
  })

  test('keeps zero values', () => {
    const result = buildBatchEditSubmission([1], {
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
    assert.equal(result.fields.length, 3)
  })

  test('builds every filled field with key and label', () => {
    const result = buildBatchEditSubmission([7], filledForm)

    assert.deepEqual(result.payload, expectedFullPayload)
    assert.deepEqual(result.fields, [
      { key: 'group', label: 'Group' },
      { key: 'tag', label: 'Tag' },
      { key: 'remark', label: 'Remark' },
      { key: 'models', label: 'Models' },
      { key: 'model_mapping', label: 'Model Mapping' },
      { key: 'weight', label: 'Weight' },
      { key: 'priority', label: 'Priority' },
      { key: 'test_model', label: 'Test Model' },
      { key: 'auto_ban', label: 'Auto Ban' },
    ])
  })

  test('skips the unchanged auto ban and an omitted model mapping', () => {
    const result = buildBatchEditSubmission([3], {
      auto_ban: 'unchanged',
      model_mapping: undefined,
      tag: 'prod',
    })

    assert.deepEqual(result.payload, { ids: [3], tag: 'prod' })
  })

  test('normalizes comma list segments before submitting', () => {
    const result = buildBatchEditSubmission([1], {
      group: ' default , vip ',
      tag: '  prod  ',
      models: ' gpt-4o , claude ',
      weight: ' 7 ',
    })

    assert.deepEqual(result.payload, {
      ids: [1],
      group: 'default,vip',
      tag: 'prod',
      models: 'gpt-4o,claude',
      weight: 7,
    })
  })

  test('accepts the inclusive length and value boundaries', () => {
    const result = buildBatchEditSubmission([1], {
      group: 'a'.repeat(64),
      tag: 'a'.repeat(191),
      remark: 'a'.repeat(255),
      test_model: 'a'.repeat(255),
      models: 'a'.repeat(255),
      weight: '4294967295',
      priority: '9007199254740991',
      model_mapping: '{}',
    })

    assert.equal(result.fields.length, 8)
  })
})
