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
import { Users } from 'lucide-react'
import { useCallback, useMemo } from 'react'
import { useTranslation } from 'react-i18next'

import { getUserQuotaDataByUsers } from '@/features/dashboard/api'
import { TopNChartsSection } from '@/features/dashboard/components/ui/topn-charts'
import {
  getDefaultDays,
  saveGranularity,
  processUserChartData,
} from '@/features/dashboard/lib'
import { useTopNTimeRange } from '@/features/dashboard/lib/use-topn-time-range'
import type { TopNChartsFilters } from '@/features/dashboard/types'
import type { TimeGranularity } from '@/lib/time'

const USER_CHARTS: {
  value: string
  labelKey: string
  specKey: 'spec_user_rank' | 'spec_user_trend'
}[] = [
  {
    value: 'rank',
    labelKey: 'User Consumption Ranking',
    specKey: 'spec_user_rank',
  },
  {
    value: 'trend',
    labelKey: 'User Consumption Trend',
    specKey: 'spec_user_trend',
  },
]

interface UserChartsProps {
  filters: TopNChartsFilters
  onFiltersChange: (filters: TopNChartsFilters) => void
}

export function UserCharts(props: UserChartsProps) {
  const { t } = useTranslation()

  // The selection is owned by the dashboard parent so it persists across
  // sub-section switches; the rolling window is derived from the chosen range.
  const selectedRange = props.filters.selectedRange
  const timeGranularity = props.filters.timeGranularity
  const topUserLimit = props.filters.topLimit
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

  const handleTopUserLimitChange = useCallback(
    (limit: number) => {
      onFiltersChange({ ...props.filters, topLimit: limit })
    },
    [onFiltersChange, props.filters]
  )

  const { data: userData, isLoading } = useQuery({
    queryKey: ['dashboard', 'user-quota', timeRange],
    queryFn: () => getUserQuotaDataByUsers(timeRange),
    select: (res) => (res.success ? (res.data ?? []) : []),
    staleTime: 60_000,
  })

  const chartData = useMemo(
    () =>
      processUserChartData(
        isLoading ? [] : (userData ?? []),
        timeGranularity,
        t,
        topUserLimit
      ),
    [userData, isLoading, timeGranularity, t, topUserLimit]
  )

  return (
    <TopNChartsSection
      icon={Users}
      chartKeyPrefix='user'
      charts={USER_CHARTS.map((chart) => ({
        value: chart.value,
        labelKey: chart.labelKey,
        spec: chartData[chart.specKey],
        barCount:
          chart.specKey === 'spec_user_rank'
            ? chartData.rankBarCount
            : undefined,
      }))}
      isLoading={isLoading}
      selectedRange={selectedRange}
      timeGranularity={timeGranularity}
      topLimit={topUserLimit}
      topLimitLabelKey='Top Users'
      onRangeChange={handleRangeChange}
      onGranularityChange={handleGranularityChange}
      onTopLimitChange={handleTopUserLimitChange}
    />
  )
}
