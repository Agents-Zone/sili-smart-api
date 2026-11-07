/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/
import assert from 'node:assert/strict'
import { describe, test } from 'node:test'

import dayjs from '@/lib/dayjs'
import { formatChartTime } from '@/lib/time'

import { buildDayHourPoints, resolveDayWindow } from '../use-topn-time-range'

const NOW = new Date('2026-11-06T10:30:45')

describe('resolveDayWindow', () => {
  test('historical day resolves to the full natural-day window', () => {
    const start = dayjs('2026-11-03').startOf('day').unix()
    const win = resolveDayWindow('2026-11-03', NOW)
    assert.equal(win?.start_timestamp, start)
    assert.equal(win?.end_timestamp, start + 86399)
  })

  test('today resolves to start-of-day through the current moment', () => {
    const win = resolveDayWindow('2026-11-06', NOW)
    assert.equal(
      win?.start_timestamp,
      dayjs('2026-11-06').startOf('day').unix()
    )
    assert.equal(win?.end_timestamp, Math.floor(NOW.getTime() / 1000))
  })

  test('future date is rejected', () => {
    assert.equal(resolveDayWindow('2026-11-07', NOW), null)
  })

  test('overflowing calendar date is rejected', () => {
    assert.equal(resolveDayWindow('2026-02-31', NOW), null)
  })

  test('empty and non-date input are rejected', () => {
    assert.equal(resolveDayWindow('', NOW), null)
    assert.equal(resolveDayWindow('not-a-date', NOW), null)
  })

  test('non-padded date segments are rejected by the format check', () => {
    assert.equal(resolveDayWindow('2026-11-6', NOW), null)
  })
})

describe('buildDayHourPoints', () => {
  test('returns 24 hourly labels spanning 00:00 to 23:00', () => {
    const points = buildDayHourPoints('2026-11-03')
    assert.equal(points.length, 24)
    assert.equal(
      points[0],
      formatChartTime(dayjs('2026-11-03').startOf('day').unix(), 'hour')
    )
    assert.equal(points[12], '11-03 12:00')
    assert.equal(points[23], '11-03 23:00')
  })

  test('invalid date yields no hour points', () => {
    assert.deepEqual(buildDayHourPoints('bad'), [])
    assert.deepEqual(buildDayHourPoints('2026-02-31'), [])
  })
})
