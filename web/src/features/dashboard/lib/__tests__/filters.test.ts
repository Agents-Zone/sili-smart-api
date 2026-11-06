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

import {
  TIME_RANGE_BY_GRANULARITY,
  TIME_RANGE_PRESETS,
} from '../../constants'
import { getDefaultDays } from '../filters'

// Guards the granularity→range coupling: switching granularity rewrites
// selectedRange to the granularity default, which must be one of the range
// presets or the range tabs lose their highlighted value (the 30-day bug).
describe('getDefaultDays', () => {
  test('every granularity default lands on a range preset', () => {
    for (const days of Object.values(TIME_RANGE_BY_GRANULARITY)) {
      assert.ok(
        TIME_RANGE_PRESETS.some((preset) => preset.days === days),
        `granularity default ${days} is not a range preset`
      )
    }
  })

  test('week granularity defaults to 29 days', () => {
    assert.equal(getDefaultDays('week'), 29)
  })
})
