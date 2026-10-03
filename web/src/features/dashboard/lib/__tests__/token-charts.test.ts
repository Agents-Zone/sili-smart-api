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
import { describe, test } from 'node:test'

import { formatChartTime } from '@/lib/time'

import type { TokenQuotaDataItem } from '../../types'
import { processTokenChartData, resolveTokenLabel } from '../charts'

const t = (k: string) => k
const DAY = 86400

function item(
  tokenId: number,
  tokenName: string,
  createdAt: number,
  quota?: number
): TokenQuotaDataItem {
  return { token_id: tokenId, token_name: tokenName, created_at: createdAt, quota }
}

describe('resolveTokenLabel', () => {
  test('token_id 0 resolves to the no-token-consumption label', () => {
    assert.equal(
      resolveTokenLabel({ token_id: 0, token_name: '', created_at: 0 }, t),
      'No token consumption'
    )
  })

  test('missing name falls back to #token_id', () => {
    assert.equal(
      resolveTokenLabel({ token_id: 101, token_name: '', created_at: 0 }, t),
      '#101'
    )
  })

  test('non-empty name wins over the fallback', () => {
    assert.equal(
      resolveTokenLabel(
        { token_id: 11, token_name: 'key-prod-01', created_at: 0 },
        t
      ),
      'key-prod-01'
    )
  })
})

describe('processTokenChartData rank', () => {
  // A aggregates to 300 across two time records; B 200; C 100.
  const rows: TokenQuotaDataItem[] = [
    item(1, 'A', DAY, 100),
    item(1, 'A', DAY * 2, 200),
    item(2, 'B', DAY, 200),
    item(3, 'C', DAY, 100),
  ]

  test('sorts by aggregated quota desc and truncates to limit', () => {
    const { spec_token_rank } = processTokenChartData(rows, 'day', t, 2)
    const values = spec_token_rank.data[0].values
    assert.equal(values.length, 2)
    assert.equal(values[0].Token, 'A')
    assert.equal(values[0].rawQuota, 300)
    assert.equal(values[1].Token, 'B')
    assert.equal(values[1].rawQuota, 200)
  })

  test('Usage converts raw quota via default quotaPerUnit', () => {
    const { spec_token_rank } = processTokenChartData(rows, 'day', t, 2)
    assert.equal(spec_token_rank.data[0].values[0].Usage, 0.0006)
  })

  test('rank subtext contains the top-N total', () => {
    const { spec_token_rank } = processTokenChartData(rows, 'day', t, 2)
    assert.ok(spec_token_rank.title.subtext.includes('Total:'))
  })

  test('token_id 0 records count toward ranking with fallback label', () => {
    const zeroRows = [item(0, '', 3600 * 5, 50), item(1, 'A', 3600 * 5, 30)]
    const { spec_token_rank } = processTokenChartData(zeroRows, 'day', t)
    const values = spec_token_rank.data[0].values
    assert.equal(values[0].Token, 'No token consumption')
    assert.equal(values[0].rawQuota, 50)
  })

  test('missing quota is treated as zero, never NaN', () => {
    const sparse = [item(1, 'A', DAY), item(2, 'B', DAY, 40)]
    const { spec_token_rank } = processTokenChartData(sparse, 'day', t)
    const values = spec_token_rank.data[0].values
    assert.equal(values.length, 2)
    assert.equal(
      values.find((v: { Token: string }) => v.Token === 'A')?.rawQuota,
      0
    )
    assert.ok(Number.isFinite(values[0].Usage))
  })
})

describe('processTokenChartData trend', () => {
  test('merges records within the same granularity bucket', () => {
    const rows = [item(1, 'A', 3600 * 5, 100), item(1, 'A', 3600 * 6, 100)]
    const { spec_token_trend } = processTokenChartData(rows, 'day', t)
    const values = spec_token_trend.data[0].values
    const dayKey = formatChartTime(3600 * 5, 'day')
    assert.equal(dayKey, formatChartTime(3600 * 6, 'day'))
    const aAtDay = values.find(
      (v: { Token: string; Time: string }) =>
        v.Token === 'A' && v.Time === dayKey
    )
    assert.ok(aAtDay)
    assert.equal(aAtDay.rawQuota, 200)
  })

  test('pads zero entries for top tokens missing at a time point', () => {
    const t1 = 3600 * 5
    const t2 = 3600 * 5 + DAY
    const rows = [
      item(1, 'A', t1, 100),
      item(1, 'A', t2, 100),
      item(2, 'B', t1, 0),
    ]
    const { spec_token_trend } = processTokenChartData(rows, 'day', t, 2)
    const values = spec_token_trend.data[0].values
    const times = Array.from(
      new Set(values.map((v: { Time: string }) => v.Time))
    )
    assert.equal(times.length, 2)
    for (const time of times) {
      const b = values.find(
        (v: { Token: string; Time: string }) =>
          v.Token === 'B' && v.Time === time
      )
      assert.ok(b, `missing B entry at ${time}`)
      assert.equal(b.rawQuota, 0)
    }
  })

  test('only includes top N series in trend values', () => {
    const rows = [item(1, 'A', DAY, 100), item(2, 'B', DAY, 50)]
    const { spec_token_trend } = processTokenChartData(rows, 'day', t, 1)
    const tokens = new Set(
      spec_token_trend.data[0].values.map((v: { Token: string }) => v.Token)
    )
    assert.deepEqual(Array.from(tokens), ['A'])
  })
})

describe('processTokenChartData empty state', () => {
  test('empty data returns empty-state specs', () => {
    const { spec_token_rank, spec_token_trend } = processTokenChartData(
      [],
      'day',
      t
    )
    assert.equal(spec_token_rank.data[0].values.length, 0)
    assert.equal(spec_token_trend.data[0].values.length, 0)
    assert.equal(spec_token_rank.title.subtext, 'No data available')
    assert.equal(spec_token_trend.title.subtext, 'No data available')
    assert.equal(spec_token_rank.type, 'bar')
    assert.equal(spec_token_trend.type, 'area')
  })
})
