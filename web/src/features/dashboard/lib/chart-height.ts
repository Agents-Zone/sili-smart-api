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

// Fixed chrome (title, axes, padding) plus per-bar vertical space for the
// horizontal Top-N rank chart, so Top 50 keeps bars legible instead of
// squeezing them into a fixed-height container.
export const RANK_CHART_BASE_HEIGHT = 120
export const RANK_CHART_PER_BAR_HEIGHT = 26
const RANK_CHART_MIN_HEIGHT = 300
const RANK_CHART_MAX_HEIGHT = 1800

export function computeRankChartHeight(barCount: number): number {
  const raw =
    RANK_CHART_BASE_HEIGHT + RANK_CHART_PER_BAR_HEIGHT * Math.max(0, barCount)
  return Math.min(Math.max(raw, RANK_CHART_MIN_HEIGHT), RANK_CHART_MAX_HEIGHT)
}
