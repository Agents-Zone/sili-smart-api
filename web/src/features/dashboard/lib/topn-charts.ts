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
import type { TFunction } from 'i18next'

import type {
  ProcessedTokenChartData,
  ProcessedUserChartData,
} from '../types'
import { USER_COLORS } from './chart-colors'
import { getCurrencyDisplay } from '@/lib/currency'
import { formatChartTime, type TimeGranularity } from '@/lib/time'

type TFn = TFunction | ((key: string) => string)

// A record in the aggregated quota stream fed into a Top-N ranking + trend
// pair: which series it belongs to (by stable id), how to show that series,
// the hour-bucketed timestamp and the consumed quota.
export interface TopNRecord {
  seriesId: string
  label: string
  createdAt: number
  quota: number
}

export interface TopNSeries {
  id: string
  label: string
}

interface TopNChartOptions {
  // VChart data id and series field name, e.g. 'userRankData' / 'User'
  dataIdPrefix: string
  seriesField: string
  rankTitleKey: string
  trendTitleKey: string
  limit: number
  t: TFn
}

function renderQuotaCompat(rawQuota: number, digits = 4): string {
  const { config, meta } = getCurrencyDisplay()
  if (meta.kind === 'tokens') return rawQuota.toLocaleString()
  const usd = rawQuota / config.quotaPerUnit
  const rate = 'exchangeRate' in meta ? meta.exchangeRate : 1
  const symbol = 'symbol' in meta ? meta.symbol : '$'
  const value = usd * rate
  const fixed = value.toFixed(digits)
  if (Number.parseFloat(fixed) === 0 && rawQuota > 0 && value > 0) {
    return symbol + Math.pow(10, -digits).toFixed(digits)
  }
  return symbol + fixed
}

/**
 * Shared aggregation core for the ranking bar + trend area chart pairs used
 * by the user and token analytics sections: totals per series, Top-N cut,
 * per-time-bucket trend values with zero padding, and both VChart specs.
 */
export function buildTopNChartSpecs(
  records: TopNRecord[],
  timeGranularity: TimeGranularity,
  options: TopNChartOptions
): { rank: Record<string, unknown>; trend: Record<string, unknown> } {
  const tt = options.t
  const limit = options.limit
  const seriesField = options.seriesField
  const { config } = getCurrencyDisplay()
  const quotaPerUnit = config.quotaPerUnit
  const formatVal = (raw: number) => renderQuotaCompat(raw, 2)

  const emptySubtext = tt('No data available')

  if (!records || records.length === 0) {
    return {
      rank: {
        type: 'bar',
        data: [{ id: `${options.dataIdPrefix}RankData`, values: [] }],
        xField: 'rawQuota',
        yField: seriesField,
        seriesField,
        direction: 'horizontal',
        title: {
          visible: true,
          text: tt(options.rankTitleKey),
          subtext: emptySubtext,
        },
        legends: { visible: false },
        color: { type: 'ordinal', range: USER_COLORS },
        background: { fill: 'transparent' },
      },
      trend: {
        type: 'area',
        data: [{ id: `${options.dataIdPrefix}TrendData`, values: [] }],
        xField: 'Time',
        yField: 'rawQuota',
        seriesField,
        title: {
          visible: true,
          text: tt(options.trendTitleKey),
          subtext: emptySubtext,
        },
        legends: { visible: true, selectMode: 'single' },
        color: { type: 'ordinal', range: USER_COLORS },
        point: { visible: false },
        background: { fill: 'transparent' },
      },
    }
  }

  const seriesQuotaTotal = new Map<string, number>()
  records.forEach((record) => {
    seriesQuotaTotal.set(
      record.seriesId,
      (seriesQuotaTotal.get(record.seriesId) || 0) + (Number(record.quota) || 0)
    )
  })

  const sorted = [...seriesQuotaTotal.entries()].sort((a, b) => b[1] - a[1])
  const topIds = sorted.slice(0, limit).map(([id]) => id)
  const topIdSet = new Set(topIds)
  const totalQuota = sorted.slice(0, limit).reduce((s, [, q]) => s + q, 0)

  const labelById = new Map<string, string>()
  records.forEach((record) => {
    if (!labelById.has(record.seriesId)) {
      labelById.set(record.seriesId, record.label)
    }
  })

  const labelOf = (id: string): string =>
    labelById.get(id) ?? id

  const rankValues = sorted.slice(0, limit).map(([id, quota]) => ({
    [seriesField]: labelOf(id),
    rawQuota: quota,
    Usage: Number((quota / quotaPerUnit).toFixed(4)),
  }))

  const colorMap = topIds.reduce<Record<string, string>>(
    (acc, id, i) => {
      acc[labelOf(id)] = USER_COLORS[i % USER_COLORS.length]
      return acc
    },
    {}
  )

  const timeSeriesMap = new Map<string, Map<string, number>>()
  const allTimePoints = new Set<string>()

  records.forEach((record) => {
    const timeKey = formatChartTime(record.createdAt, timeGranularity)
    allTimePoints.add(timeKey)
    if (!topIdSet.has(record.seriesId)) return
    const map = timeSeriesMap.get(timeKey) ?? new Map<string, number>()
    timeSeriesMap.set(timeKey, map)
    map.set(
      record.seriesId,
      (map.get(record.seriesId) || 0) + (Number(record.quota) || 0)
    )
  })

  const sortedTimePoints = [...allTimePoints].sort()
  const trendValues = sortedTimePoints.flatMap((time) =>
    topIds.map((id) => {
      const q = timeSeriesMap.get(time)?.get(id) || 0
      return {
        Time: time,
        [seriesField]: labelOf(id),
        rawQuota: q,
        Usage: Number((q / quotaPerUnit).toFixed(4)),
      }
    })
  )

  const totalSubtext = `${tt('Total:')} ${formatVal(totalQuota)}`

  return {
    rank: {
      type: 'bar',
      data: [{ id: `${options.dataIdPrefix}RankData`, values: rankValues }],
      xField: 'rawQuota',
      yField: seriesField,
      seriesField,
      direction: 'horizontal',
      title: {
        visible: true,
        text: tt(options.rankTitleKey),
        subtext: totalSubtext,
      },
      legends: { visible: false },
      bar: {
        state: { hover: { stroke: '#000', lineWidth: 1 } },
      },
      label: {
        visible: true,
        position: 'outside',
        formatMethod: (value: number) => formatVal(value),
        style: { fontSize: 11 },
      },
      axes: [
        { orient: 'left', type: 'band' },
        { orient: 'bottom', type: 'linear', visible: false },
      ],
      tooltip: {
        mark: {
          content: [
            {
              key: (datum: Record<string, unknown>) => datum?.[seriesField],
              value: (datum: Record<string, unknown>) =>
                formatVal(Number(datum?.rawQuota) || 0),
            },
          ],
          updateContent: (
            array: Array<{
              key: string
              value: string | number
              datum?: Record<string, unknown>
            }>
          ) => {
            for (let i = 0; i < array.length; i++) {
              const rawQuota = array[i].datum?.rawQuota
              const value =
                rawQuota === undefined ? array[i].value : Number(rawQuota)
              array[i].value = formatVal(Number(value) || 0)
            }
            return array
          },
        },
      },
      color: { specified: colorMap },
      background: { fill: 'transparent' },
      animation: true,
    },
    trend: {
      type: 'area',
      data: [{ id: `${options.dataIdPrefix}TrendData`, values: trendValues }],
      xField: 'Time',
      yField: 'rawQuota',
      seriesField,
      stack: false,
      title: {
        visible: true,
        text: tt(options.trendTitleKey),
        subtext: totalSubtext,
      },
      legends: { visible: true, selectMode: 'single' },
      axes: [
        { orient: 'bottom', type: 'band' },
        {
          orient: 'left',
          type: 'linear',
          label: {
            formatMethod: (value: number) => formatVal(value),
          },
        },
      ],
      tooltip: {
        mark: {
          content: [
            {
              key: (datum: Record<string, unknown>) => datum?.[seriesField],
              value: (datum: Record<string, unknown>) =>
                formatVal(Number(datum?.rawQuota) || 0),
            },
          ],
        },
        dimension: {
          content: [
            {
              key: (datum: Record<string, unknown>) => datum?.[seriesField],
              value: (datum: Record<string, unknown>) =>
                Number(datum?.rawQuota) || 0,
            },
          ],
          updateContent: (
            array: Array<{
              key: string
              value: string | number
            }>
          ) => {
            array.sort(
              (a, b) => (Number(b.value) || 0) - (Number(a.value) || 0)
            )
            let sum = 0
            for (let i = 0; i < array.length; i++) {
              const v = Number(array[i].value) || 0
              sum += v
              array[i].value = formatVal(v)
            }
            array.unshift({
              key: tt('Total:'),
              value: formatVal(sum),
            })
            return array
          },
        },
      },
      area: {
        style: {
          fillOpacity: 0.15,
          curveType: 'monotone',
        },
      },
      line: {
        style: {
          lineWidth: 2,
          curveType: 'monotone',
        },
      },
      point: { visible: false },
      color: { specified: colorMap },
      background: { fill: 'transparent' },
      animation: true,
    },
  }
}

// Kept for the exported signatures used by index.ts; the specs themselves
// come from the shared core above.
export function buildUserChartSpecs(
  records: TopNRecord[],
  timeGranularity: TimeGranularity,
  t: TFn,
  limit: number
): ProcessedUserChartData {
  const { rank, trend } = buildTopNChartSpecs(records, timeGranularity, {
    dataIdPrefix: 'user',
    seriesField: 'User',
    rankTitleKey: 'User Consumption Ranking',
    trendTitleKey: 'User Consumption Trend',
    limit,
    t,
  })
  return { spec_user_rank: rank, spec_user_trend: trend }
}

export function buildTokenChartSpecs(
  records: TopNRecord[],
  timeGranularity: TimeGranularity,
  t: TFn,
  limit: number
): ProcessedTokenChartData {
  const { rank, trend } = buildTopNChartSpecs(records, timeGranularity, {
    dataIdPrefix: 'token',
    seriesField: 'Token',
    rankTitleKey: 'Token Consumption Ranking',
    trendTitleKey: 'Token Consumption Trend',
    limit,
    t,
  })
  return { spec_token_rank: rank, spec_token_trend: trend }
}
