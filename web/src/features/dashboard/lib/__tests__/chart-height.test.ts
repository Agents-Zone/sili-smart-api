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

import {
  RANK_CHART_BASE_HEIGHT,
  RANK_CHART_PER_BAR_HEIGHT,
  computeRankChartHeight,
} from '../chart-height'

describe('computeRankChartHeight', () => {
  test('clamps to the minimum for empty/loading states', () => {
    assert.equal(computeRankChartHeight(0), 300)
  })

  test('scales linearly with the bar count', () => {
    assert.equal(
      computeRankChartHeight(10),
      RANK_CHART_BASE_HEIGHT + RANK_CHART_PER_BAR_HEIGHT * 10
    )
    assert.equal(computeRankChartHeight(10), 380)
    assert.equal(computeRankChartHeight(50), 1420)
  })

  test('clamps to the maximum for extreme counts', () => {
    assert.equal(computeRankChartHeight(10_000), 1800)
  })

  test('negative counts fall back to the minimum', () => {
    assert.equal(computeRankChartHeight(-5), 300)
  })
})
