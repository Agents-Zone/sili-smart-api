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
import { useQuery } from '@tanstack/react-query'
import { KeyRound } from 'lucide-react'
import { useCallback, useMemo } from 'react'
import { useTranslation } from 'react-i18next'

import { getTokenQuotaDataByTokens } from '@/features/dashboard/api'
import { TopNChartsSection } from '@/features/dashboard/components/ui/topn-charts'
import {
  getDefaultDays,
  saveGranularity,
  processTokenChartData,
} from '@/features/dashboard/lib'
import { useTopNTimeRange } from '@/features/dashboard/lib/use-topn-time-range'
import type { TopNChartsFilters } from '@/features/dashboard/types'
import type { TimeGranularity } from '@/lib/time'

const TOKEN_CHARTS: {
  value: string
  labelKey: string
  specKey: 'spec_token_rank' | 'spec_token_trend'
}[] = [
  {
    value: 'rank',
    labelKey: 'Token Usage Ranking',
    specKey: 'spec_token_rank',
  },
  {
    value: 'trend',
    labelKey: 'Token Usage Trend',
    specKey: 'spec_token_trend',
  },
]

interface TokenChartsProps {
  filters: TopNChartsFilters
  onFiltersChange: (filters: TopNChartsFilters) => void
}

export function TokenCharts(props: TokenChartsProps) {
  const { t } = useTranslation()

  // The selection is owned by the dashboard parent so it persists across
  // sub-section switches; the rolling window is derived from the chosen range.
  const selectedRange = props.filters.selectedRange
  const timeGranularity = props.filters.timeGranularity
  const topTokenLimit = props.filters.topLimit
  const onFiltersChange = props.onFiltersChange

  const timeRange = useTopNTimeRange(selectedRange)

  const handleRangeChange = useCallback(
    (days: number) => {
      onFiltersChange({ ...props.filters, selectedRange: days })
    },
    [onFiltersChange, props.filters]
  )

  const handleGranularityChange = useCallback(
    (g: TimeGranularity) => {
      saveGranularity(g)
      onFiltersChange({
        ...props.filters,
        timeGranularity: g,
        selectedRange: getDefaultDays(g),
      })
    },
    [onFiltersChange, props.filters]
  )

  const handleTopTokenLimitChange = useCallback(
    (limit: number) => {
      onFiltersChange({ ...props.filters, topLimit: limit })
    },
    [onFiltersChange, props.filters]
  )

  const { data: tokenData, isLoading } = useQuery({
    queryKey: ['dashboard', 'token-quota', timeRange],
    queryFn: () => getTokenQuotaDataByTokens(timeRange),
    select: (res) => (res.success ? (res.data ?? []) : []),
    staleTime: 60_000,
  })

  const chartData = useMemo(
    () =>
      processTokenChartData(
        isLoading ? [] : (tokenData ?? []),
        timeGranularity,
        t,
        topTokenLimit
      ),
    [tokenData, isLoading, timeGranularity, t, topTokenLimit]
  )

  return (
    <TopNChartsSection
      icon={KeyRound}
      chartKeyPrefix='token'
      charts={TOKEN_CHARTS.map((chart) => ({
        value: chart.value,
        labelKey: chart.labelKey,
        spec: chartData[chart.specKey],
        barCount:
          chart.specKey === 'spec_token_rank'
            ? chartData.rankBarCount
            : undefined,
      }))}
      isLoading={isLoading}
      selectedRange={selectedRange}
      timeGranularity={timeGranularity}
      topLimit={topTokenLimit}
      topLimitLabelKey='Top Tokens'
      onRangeChange={handleRangeChange}
      onGranularityChange={handleGranularityChange}
      onTopLimitChange={handleTopTokenLimitChange}
    />
  )
}
