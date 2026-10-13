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

import { formatTokensInM } from './format'

describe('formatTokensInM', () => {
  test('formats zero', () => {
    assert.equal(formatTokensInM(0), '0.00 M')
  })

  test('keeps two decimals for sub-million counts', () => {
    assert.equal(formatTokensInM(9_999), '0.01 M')
  })

  test('formats whole millions', () => {
    assert.equal(formatTokensInM(1_500_000), '1.50 M')
  })

  test('rounds half away from zero via toFixed', () => {
    assert.equal(formatTokensInM(2_055_000), '2.06 M')
  })
})
