/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even implied warranties of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/
import assert from 'node:assert/strict'
import { describe, test } from 'node:test'

import type { QuotaDataItem } from '../../types'
import { processUserChartData } from '../charts'

const t = (k: string) => k
const DAY = 86400

// Guards the user analytics section against the token-metric refactor: it
// must keep aggregating and displaying quota (currency), not token_used.
describe('processUserChartData', () => {
  // alice aggregates to 300K quota across two records; bob 200K.
  const rows: QuotaDataItem[] = [
    { username: 'alice', created_at: DAY, quota: 100_000, token_used: 999 },
    { username: 'alice', created_at: DAY * 2, quota: 200_000, token_used: 1 },
    { username: 'bob', created_at: DAY, quota: 200_000, token_used: 5 },
  ]

  test('ranks by aggregated quota desc, ignoring token_used', () => {
    const { spec_user_rank } = processUserChartData(rows, 'day', t, 2)
    const values = spec_user_rank.data[0].values
    assert.equal(values.length, 2)
    assert.equal(values[0].User, 'alice')
    assert.equal(values[0].value, 300_000)
    assert.equal(values[1].User, 'bob')
    assert.equal(values[1].value, 200_000)
  })

  test('subtext shows the top-N total as currency', () => {
    const { spec_user_rank } = processUserChartData(rows, 'day', t, 2)
    assert.match(spec_user_rank.title.subtext, /^Total: \$[\d.]+$/)
  })

  test('rankBarCount mirrors the number of ranked bars', () => {
    assert.equal(processUserChartData(rows, 'day', t, 2).rankBarCount, 2)
    assert.equal(processUserChartData([], 'day', t).rankBarCount, 0)
  })

  test('trend values carry the quota metric per time bucket', () => {
    const { spec_user_trend } = processUserChartData(rows, 'day', t, 2)
    const values = spec_user_trend.data[0].values
    const alice = values.find(
      (v: { User: string; Time: string }) => v.User === 'alice'
    )
    assert.ok(alice)
    assert.equal(typeof alice.value, 'number')
  })
})
