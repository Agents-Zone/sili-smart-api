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
import { dataScheme as vchartDefaultDataScheme } from '@visactor/vchart/esm/theme/color-scheme/builtin/default'

import { MAX_CHART_TREND_POINTS } from '@/features/dashboard/constants'
import type {
  QuotaDataItem,
  ProcessedChartData,
  ProcessedTokenChartData,
  ProcessedUserChartData,
  TokenQuotaDataItem,
} from '@/features/dashboard/types'
import {
  buildTokenChartSpecs,
  buildUserChartSpecs,
  renderQuotaCompat,
  type TopNRecord,
} from './topn-charts'
import { getCurrencyDisplay } from '@/lib/currency'
import { formatChartTime, type TimeGranularity } from '@/lib/time'

type TFunction = (key: string) => string
type TooltipLineItem = {
  key: string
  value: string | number
  datum?: Record<string, unknown>
  hasShape?: boolean
  shapeType?: string
  shapeFill?: string
  shapeStroke?: string
  shapeSize?: number
}

export function getDashboardChartColors(domainLength: number): string[] {
  const scheme =
    vchartDefaultDataScheme.find(
      (item) => !item.maxDomainLength || domainLength <= item.maxDomainLength
    ) ?? vchartDefaultDataScheme[vchartDefaultDataScheme.length - 1]

  return scheme.scheme.filter(
    (color): color is string => typeof color === 'string'
  )
}

/**
 * Process and aggregate chart data
 */
export function processChartData(
  data: QuotaDataItem[],
  timeGranularity: TimeGranularity = 'day',
  t?: TFunction,
  chartCornerRadius?: number
): ProcessedChartData {
  const tt: TFunction = t ?? ((x) => x)
  const otherLabel = tt('Other')

  const formatInt = (value: number) =>
    Intl.NumberFormat(undefined, { maximumFractionDigits: 0 }).format(value)
  const formatQuotaValue = (value: number) => renderQuotaCompat(value, 4)
  const formatQuotaTotal = (value: number) => renderQuotaCompat(value, 2)

  const MAX_TOOLTIP_MODELS = 15
  const isOtherTooltipKey = (key: string) =>
    key === 'Other' || key === otherLabel

  const makeTooltipDimensionUpdateContent = (options?: {
    collapseOverflow?: boolean
  }) => {
    const collapseOverflow = options?.collapseOverflow ?? true

    return (array: TooltipLineItem[]) => {
      const modelItems = array.filter((item) => !isOtherTooltipKey(item.key))
      const otherItems = array.filter((item) => isOtherTooltipKey(item.key))
      modelItems.sort((a, b) => (Number(b.value) || 0) - (Number(a.value) || 0))
      array = [...modelItems, ...otherItems]

      let sum = 0
      for (let i = 0; i < array.length; i++) {
        const v = Number(array[i].value) || 0
        if (
          array[i].datum &&
          (array[i].datum as Record<string, unknown>)?.TimeSum
        ) {
          sum =
            Number((array[i].datum as Record<string, unknown>)?.TimeSum) || sum
        }
        array[i].value = formatQuotaValue(v)
      }

      if (collapseOverflow && array.length > MAX_TOOLTIP_MODELS) {
        const visible = modelItems.slice(0, MAX_TOOLTIP_MODELS)
        const otherSum = [
          ...modelItems.slice(MAX_TOOLTIP_MODELS),
          ...otherItems,
        ].reduce((sum, item) => {
          const raw = item.datum
            ? Number((item.datum as Record<string, unknown>)?.rawQuota) || 0
            : 0
          return sum + raw
        }, 0)
        array = [
          ...visible,
          {
            key: otherLabel,
            value: formatQuotaValue(otherSum),
            hasShape: true,
            shapeType: 'square',
            shapeFill: otherTooltipColor,
            shapeStroke: otherTooltipColor,
            shapeSize: 8,
          },
        ]
      }

      array.unshift({
        key: tt('Total:'),
        value: formatQuotaValue(sum),
      })
      return array
    }
  }

  if (!data || data.length === 0) {
    return {
      spec_pie: {
        type: 'pie',
        data: [{ id: 'id0', values: [] }],
        outerRadius: 0.8,
        innerRadius: 0.5,
        padAngle: 0.6,
        valueField: 'value',
        categoryField: 'type',
        title: {
          visible: true,
          text: tt('Call Count Distribution'),
          subtext: tt('No data available'),
        },
        legends: { visible: false },
        label: { visible: false },
        tooltip: {
          mark: {
            content: [],
          },
        },
      },
      spec_line: {
        type: 'bar',
        data: [{ id: 'barData', values: [] }],
        xField: 'Time',
        yField: 'Usage',
        seriesField: 'Model',
        stack: true,
        legends: { visible: true, selectMode: 'single' },
      },
      spec_area: {
        type: 'area',
        data: [{ id: 'areaData', values: [] }],
        xField: 'Time',
        yField: 'Usage',
        seriesField: 'Model',
        stack: true,
        legends: { visible: true, selectMode: 'single' },
      },
      spec_model_line: {
        type: 'area',
        data: [{ id: 'lineData', values: [] }],
        xField: 'Time',
        yField: 'Count',
        seriesField: 'Model',
        legends: { visible: true, selectMode: 'single' },
        title: {
          visible: true,
          text: tt('Call Trend'),
        },
      },
      spec_rank_bar: {
        type: 'bar',
        data: [{ id: 'rankData', values: [] }],
        xField: 'Model',
        yField: 'Count',
        seriesField: 'Model',
        legends: { visible: true, selectMode: 'single' },
        title: {
          visible: true,
          text: tt('Call Count Ranking'),
        },
      },
      totalQuotaDisplay: formatQuotaTotal(0),
      totalCountDisplay: formatInt(0),
    }
  }

  const { config } = getCurrencyDisplay()
  const quotaPerUnit = config.quotaPerUnit

  // Aggregate all metrics by time and model
  const timeModelMap = new Map<
    string,
    Map<string, { quota: number; count: number; tokens: number }>
  >()
  const modelTotalsMap = new Map<
    string,
    { quota: number; count: number; tokens: number }
  >()

  data.forEach((item) => {
    const timestamp = Number(item.created_at)
    const timeKey = formatChartTime(timestamp, timeGranularity)
    const model = item.model_name || 'Unknown'
    const quota = Number(item.quota) || 0
    const count = Number(item.count) || 0
    const tokens = Number(item.token_used) || 0

    // Aggregate by time and model
    if (!timeModelMap.has(timeKey)) {
      timeModelMap.set(timeKey, new Map())
    }
    const modelMap = timeModelMap.get(timeKey)!
    const existing = modelMap.get(model) || { quota: 0, count: 0, tokens: 0 }
    modelMap.set(model, {
      quota: existing.quota + quota,
      count: existing.count + count,
      tokens: existing.tokens + tokens,
    })

    // Calculate totals
    const totalExisting = modelTotalsMap.get(model) || {
      quota: 0,
      count: 0,
      tokens: 0,
    }
    modelTotalsMap.set(model, {
      quota: totalExisting.quota + quota,
      count: totalExisting.count + count,
      tokens: totalExisting.tokens + tokens,
    })
  })

  const allModels = Array.from(modelTotalsMap.keys())
  const sortedTimes = Array.from(timeModelMap.keys()).sort()
  const sortedModels = [...allModels].sort()
  const modelColorDomain = Array.from(new Set([...sortedModels, otherLabel]))
  const modelColorRange = getDashboardChartColors(modelColorDomain.length)
  const otherColor = modelColorRange[modelColorDomain.indexOf(otherLabel)]
  const otherTooltipColor =
    typeof otherColor === 'string' ? otherColor : '#FF8A00'
  const modelColor = {
    type: 'ordinal',
    domain: modelColorDomain,
    range: modelColorRange,
  }

  // Pad time points if too few (default 7 points)
  const MAX_TREND_POINTS = MAX_CHART_TREND_POINTS
  const fillTimePoints = (times: string[]) => {
    if (times.length >= MAX_TREND_POINTS) return times
    const lastTime = Math.max(
      ...data.map((item) => Number(item.created_at) || 0)
    )
    const intervalSec =
      timeGranularity === 'week'
        ? 604800
        : timeGranularity === 'day'
          ? 86400
          : 3600
    const padded = Array.from({ length: MAX_TREND_POINTS }, (_, i) =>
      formatChartTime(
        lastTime - (MAX_TREND_POINTS - 1 - i) * intervalSec,
        timeGranularity
      )
    )
    return padded
  }
  const chartTimes = fillTimePoints(sortedTimes)

  const totalTimes = Array.from(modelTotalsMap.values()).reduce(
    (sum, x) => sum + (Number(x.count) || 0),
    0
  )
  const totalQuotaRaw = Array.from(modelTotalsMap.values()).reduce(
    (sum, x) => sum + (Number(x.quota) || 0),
    0
  )

  // Pie chart (model call count proportion)
  const pieValues = Array.from(modelTotalsMap.entries())
    .map(([model, stats]) => ({
      type: model,
      value: Number(stats.count) || 0,
    }))
    .sort((a, b) => b.value - a.value)

  // Stacked bar: model quota distribution (quota -> USD)
  const lineValues: Array<{
    Time: string
    Model: string
    rawQuota: number
    Usage: number
    TimeSum: number
  }> = []

  chartTimes.forEach((time) => {
    let timeData = sortedModels.map((model) => {
      const stats = timeModelMap.get(time)?.get(model)
      const rawQuota = Number(stats?.quota) || 0
      const usd = rawQuota ? rawQuota / quotaPerUnit : 0
      // Match legacy frontend getQuotaWithUnit(..., 4)
      const usage = usd ? Number(usd.toFixed(4)) : 0
      return {
        Time: time,
        Model: model,
        rawQuota,
        Usage: usage,
        TimeSum: 0,
      }
    })

    const timeSum = timeData.reduce((sum, item) => sum + item.rawQuota, 0)
    timeData.sort((a, b) => b.rawQuota - a.rawQuota)
    timeData = timeData.map((item) => ({ ...item, TimeSum: timeSum }))
    lineValues.push(...timeData)
  })
  lineValues.sort((a, b) => a.Time.localeCompare(b.Time))

  // Area chart: top models by quota + "Other" bucket (too many series = unreadable)
  const MAX_AREA_MODELS = 15
  const rankedQuotaModels = Array.from(modelTotalsMap.entries())
    .map(([model, stats]) => ({
      Model: model,
      Quota: Number(stats.quota) || 0,
    }))
    .sort((a, b) => b.Quota - a.Quota)
  const topAreaModels = new Set(
    rankedQuotaModels.slice(0, MAX_AREA_MODELS).map((m) => m.Model)
  )

  const areaValues: typeof lineValues = []
  chartTimes.forEach((time) => {
    const buckets = new Map<string, { rawQuota: number; usage: number }>()
    const modelMap = timeModelMap.get(time)
    let timeSum = 0
    sortedModels.forEach((model) => {
      const stats = modelMap?.get(model)
      const rawQuota = Number(stats?.quota) || 0
      const usd = rawQuota ? rawQuota / quotaPerUnit : 0
      const usage = usd ? Number(usd.toFixed(4)) : 0
      timeSum += rawQuota
      const key = topAreaModels.has(model) ? model : otherLabel
      const prev = buckets.get(key) || { rawQuota: 0, usage: 0 }
      buckets.set(key, {
        rawQuota: prev.rawQuota + rawQuota,
        usage: Number((prev.usage + usage).toFixed(4)),
      })
    })
    for (const [model, vals] of buckets) {
      areaValues.push({
        Time: time,
        Model: model,
        rawQuota: vals.rawQuota,
        Usage: vals.usage,
        TimeSum: timeSum,
      })
    }
  })
  areaValues.sort((a, b) => a.Time.localeCompare(b.Time))

  // Line chart: model call trend (top models + "Other" bucket)
  const MAX_TREND_MODELS = 20
  const rankedTrendModels = Array.from(modelTotalsMap.entries())
    .map(([model, stats]) => ({
      Model: model,
      Count: Number(stats.count) || 0,
    }))
    .sort((a, b) => b.Count - a.Count)
  const topTrendModels = rankedTrendModels
    .slice(0, MAX_TREND_MODELS)
    .map((item) => item.Model)
  const otherTrendModels = rankedTrendModels
    .slice(MAX_TREND_MODELS)
    .map((item) => item.Model)

  const modelLineValues: Array<{
    Time: string
    Model: string
    Count: number
  }> = []
  chartTimes.forEach((time) => {
    const timeData = topTrendModels.map((model) => {
      const stats = timeModelMap.get(time)?.get(model)
      return {
        Time: time,
        Model: model,
        Count: Number(stats?.count) || 0,
      }
    })
    if (otherTrendModels.length > 0) {
      const otherCount = otherTrendModels.reduce((sum, model) => {
        const stats = timeModelMap.get(time)?.get(model)
        return sum + (Number(stats?.count) || 0)
      }, 0)
      timeData.push({
        Time: time,
        Model: otherLabel,
        Count: otherCount,
      })
    }
    modelLineValues.push(...timeData)
  })
  modelLineValues.sort((a, b) => a.Time.localeCompare(b.Time))

  // Rank bar: model call count ranking (top 20 + "Other" bucket)
  const MAX_RANK_MODELS = 20
  const allRankValues = Array.from(modelTotalsMap.entries())
    .map(([model, stats]) => ({
      Model: model,
      Count: Number(stats.count) || 0,
    }))
    .sort((a, b) => b.Count - a.Count)

  let rankValues: typeof allRankValues
  if (allRankValues.length > MAX_RANK_MODELS) {
    const topModels = allRankValues.slice(0, MAX_RANK_MODELS)
    const otherCount = allRankValues
      .slice(MAX_RANK_MODELS)
      .reduce((sum, item) => sum + item.Count, 0)
    rankValues = [...topModels, { Model: otherLabel, Count: otherCount }]
  } else {
    rankValues = allRankValues
  }

  return {
    spec_pie: {
      type: 'pie',
      data: [{ id: 'id0', values: pieValues }],
      outerRadius: 0.8,
      innerRadius: 0.5,
      padAngle: 0.6,
      valueField: 'value',
      categoryField: 'type',
      pie: {
        style:
          chartCornerRadius == null ? {} : { cornerRadius: chartCornerRadius },
        state: {
          hover: { outerRadius: 0.85, stroke: '#000', lineWidth: 1 },
          selected: { outerRadius: 0.85, stroke: '#000', lineWidth: 1 },
        },
      },
      title: {
        visible: true,
        text: tt('Call Count Distribution'),
      },
      legends: { visible: true, orient: 'left' },
      label: { visible: true },
      color: modelColor,
      tooltip: {
        mark: {
          content: [
            {
              key: (datum: Record<string, unknown>) => datum?.type,
              value: (datum: Record<string, unknown>) =>
                formatInt(Number(datum?.value) || 0),
            },
          ],
        },
      },
      background: { fill: 'transparent' },
      animation: true,
    },
    spec_line: {
      type: 'bar',
      data: [{ id: 'barData', values: lineValues }],
      xField: 'Time',
      yField: 'Usage',
      seriesField: 'Model',
      stack: true,
      legends: { visible: true, selectMode: 'single' },
      color: modelColor,
      bar: {
        state: {
          hover: { stroke: '#000', lineWidth: 1 },
        },
      },
      tooltip: {
        mark: {
          content: [
            {
              key: (datum: Record<string, unknown>) => datum?.Model,
              value: (datum: Record<string, unknown>) =>
                formatQuotaValue(Number(datum?.rawQuota) || 0),
            },
          ],
        },
        dimension: {
          content: [
            {
              key: (datum: Record<string, unknown>) => datum?.Model,
              value: (datum: Record<string, unknown>) =>
                Number(datum?.rawQuota) || 0,
            },
          ],
          updateContent: makeTooltipDimensionUpdateContent(),
        },
      },
      background: { fill: 'transparent' },
      animation: true,
    },
    spec_area: {
      type: 'area',
      data: [{ id: 'areaData', values: areaValues }],
      xField: 'Time',
      yField: 'Usage',
      seriesField: 'Model',
      stack: false,
      legends: { visible: true, selectMode: 'single' },
      color: modelColor,
      tooltip: {
        mark: {
          content: [
            {
              key: (datum: Record<string, unknown>) => datum?.Model,
              value: (datum: Record<string, unknown>) =>
                formatQuotaValue(Number(datum?.rawQuota) || 0),
            },
          ],
        },
        dimension: {
          content: [
            {
              key: (datum: Record<string, unknown>) => datum?.Model,
              value: (datum: Record<string, unknown>) =>
                Number(datum?.rawQuota) || 0,
            },
          ],
          updateContent: makeTooltipDimensionUpdateContent({
            collapseOverflow: false,
          }),
        },
      },
      area: {
        style: {
          fillOpacity: 0.08,
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
      background: { fill: 'transparent' },
      animation: true,
    },
    spec_model_line: {
      type: 'area',
      data: [{ id: 'lineData', values: modelLineValues }],
      xField: 'Time',
      yField: 'Count',
      seriesField: 'Model',
      stack: false,
      legends: { visible: true, selectMode: 'single' },
      color: modelColor,
      title: {
        visible: true,
        text: tt('Call Trend'),
      },
      tooltip: {
        mark: {
          content: [
            {
              key: (datum: Record<string, unknown>) => datum?.Model,
              value: (datum: Record<string, unknown>) =>
                formatInt(Number(datum?.Count) || 0),
            },
          ],
        },
        dimension: {
          content: [
            {
              key: (datum: Record<string, unknown>) => datum?.Model,
              value: (datum: Record<string, unknown>) =>
                Number(datum?.Count) || 0,
            },
          ],
          updateContent: (
            array: Array<{
              key: string
              value: string | number
            }>
          ) => {
            const modelItems = array.filter(
              (item) => !isOtherTooltipKey(item.key)
            )
            const otherItems = array.filter((item) =>
              isOtherTooltipKey(item.key)
            )
            modelItems.sort(
              (a, b) => (Number(b.value) || 0) - (Number(a.value) || 0)
            )
            array = [...modelItems, ...otherItems]

            let sum = 0
            for (let i = 0; i < array.length; i++) {
              const v = Number(array[i].value) || 0
              sum += v
              array[i].value = formatInt(v)
            }
            array.unshift({
              key: tt('Total:'),
              value: formatInt(sum),
            })
            return array
          },
        },
      },
      area: {
        style: {
          fillOpacity: 0.08,
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
      background: { fill: 'transparent' },
      animation: true,
    },
    spec_rank_bar: {
      type: 'bar',
      data: [{ id: 'rankData', values: rankValues }],
      xField: 'Model',
      yField: 'Count',
      seriesField: 'Model',
      legends: { visible: true, selectMode: 'single' },
      color: modelColor,
      title: {
        visible: true,
        text: tt('Call Count Ranking'),
      },
      bar: {
        state: {
          hover: { stroke: '#000', lineWidth: 1 },
        },
      },
      tooltip: {
        mark: {
          content: [
            {
              key: (datum: Record<string, unknown>) => datum?.Model,
              value: (datum: Record<string, unknown>) =>
                formatInt(Number(datum?.Count) || 0),
            },
          ],
        },
      },
      background: { fill: 'transparent' },
      animation: true,
    },
    totalQuotaDisplay: formatQuotaTotal(totalQuotaRaw),
    totalCountDisplay: formatInt(totalTimes),
  }
}

export function processUserChartData(
  data: QuotaDataItem[],
  timeGranularity: TimeGranularity = 'day',
  t?: TFunction,
  limit = 10
): ProcessedUserChartData {
  const tt: TFunction = t ?? ((x) => x)
  const records: TopNRecord[] = (data ?? []).map((item) => ({
    seriesId: item.username || 'unknown',
    label: item.username || 'unknown',
    createdAt: Number(item.created_at),
    value: Number(item.quota) || 0,
  }))
  return buildUserChartSpecs(records, timeGranularity, tt, limit)
}

export function resolveTokenLabel(
  item: TokenQuotaDataItem,
  t: TFunction
): string {
  if (item.token_id === 0) return t('No token consumption')
  return item.token_name || `#${item.token_id}`
}

// Series identity for token charts: tokens are grouped by token_id, not by
// display label, so distinct tokens sharing a name stay separate series.
interface TokenSeries {
  tokenId: number
  label: string
}

export function processTokenChartData(
  data: TokenQuotaDataItem[],
  timeGranularity: TimeGranularity = 'day',
  t?: TFunction,
  limit = 10
): ProcessedTokenChartData {
  const tt: TFunction = t ?? ((x) => x)

  // Group by token_id (spec §4.1.4 rule 1). Labels may collide across
  // token_ids; duplicate display labels get a "#id" suffix to stay readable.
  const seriesByToken = new Map<number, TokenSeries>()
  const labelCounts = new Map<string, number>()
  const records: TopNRecord[] = (data ?? []).map((item) => {
    if (!seriesByToken.has(item.token_id)) {
      const baseLabel = resolveTokenLabel(item, tt)
      const seen = labelCounts.get(baseLabel) || 0
      labelCounts.set(baseLabel, seen + 1)
      const label = seen > 0 ? `${baseLabel} #${item.token_id}` : baseLabel
      seriesByToken.set(item.token_id, { tokenId: item.token_id, label })
    }
    const series = seriesByToken.get(item.token_id) ?? {
      tokenId: item.token_id,
      label: resolveTokenLabel(item, tt),
    }
    return {
      seriesId: String(item.token_id),
      label: series.label,
      createdAt: Number(item.created_at),
      value: Number(item.token_used) || 0,
    }
  })
  return buildTokenChartSpecs(records, timeGranularity, tt, limit)
}
