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
import { VChart } from '@visactor/react-vchart'
import { Loader2, type LucideIcon } from 'lucide-react'
import { useCallback } from 'react'
import { useTranslation } from 'react-i18next'

import { DatePicker } from '@/components/date-picker'
import { IconBadge } from '@/components/ui/icon-badge'
import { Skeleton } from '@/components/ui/skeleton'
import { Tabs, TabsList, TabsTrigger } from '@/components/ui/tabs'
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from '@/components/ui/tooltip'
import {
  TIME_GRANULARITY_OPTIONS,
  TIME_RANGE_PRESETS,
  TOP_N_LIMIT_OPTIONS,
} from '@/features/dashboard/constants'
import { computeRankChartHeight } from '@/features/dashboard/lib/chart-height'
import dayjs from '@/lib/dayjs'
import type { TimeGranularity } from '@/lib/time'
import { useChartTheme } from '@/lib/use-chart-theme'
import { VCHART_OPTION } from '@/lib/vchart'

export interface TopNChartCard {
  value: string
  labelKey: string
  spec: Record<string, unknown> | undefined
  // Present only on horizontal bar charts: the number of bars drives the
  // adaptive card height. Absent keeps the fixed default height.
  barCount?: number
}

export interface TopNChartsSectionProps {
  icon: LucideIcon
  charts: TopNChartCard[]
  chartKeyPrefix: string
  isLoading: boolean
  selectedRange: number
  timeGranularity: TimeGranularity
  topLimit: number
  topLimitLabelKey: string
  onRangeChange: (days: number) => void
  onGranularityChange: (granularity: TimeGranularity) => void
  onTopLimitChange: (limit: number) => void
  selectedDate?: string
  onSelectedDateChange?: (date: string | undefined) => void
  granularityDisabled?: boolean
  granularityDisabledTitleKey?: string
}

/**
 * Shared layout for the Top-N analytics sections (users / tokens): the
 * filter bar (range, granularity, Top-N) plus one bordered chart card per
 * spec, with theme-following VChart rendering.
 */
export function TopNChartsSection(props: TopNChartsSectionProps) {
  const { t } = useTranslation()
  const { resolvedTheme, themeReady } = useChartTheme()

  const handleRangeChange = useCallback(
    (value: string) => {
      props.onRangeChange(Number(value))
    },
    [props]
  )
  const handleGranularityChange = useCallback(
    (value: string) => {
      props.onGranularityChange(value as TimeGranularity)
    },
    [props]
  )
  const handleTopLimitChange = useCallback(
    (value: string) => {
      props.onTopLimitChange(Number(value))
    },
    [props]
  )
  const handleSelectedDateSelect = useCallback(
    (date: Date | undefined) => {
      props.onSelectedDateChange?.(
        date ? dayjs(date).format('YYYY-MM-DD') : undefined
      )
    },
    [props]
  )
  const handleSelectedDateClear = useCallback(() => {
    props.onSelectedDateChange?.(undefined)
  }, [props])

  const dateActive = Boolean(props.selectedDate)

  const granularityTabs = (
    <Tabs
      value={props.timeGranularity}
      onValueChange={handleGranularityChange}
      className='shrink-0'
    >
      <TabsList>
        {TIME_GRANULARITY_OPTIONS.map((opt) => (
          <TabsTrigger
            key={opt.value}
            value={opt.value}
            className='px-2.5 text-xs'
            disabled={props.granularityDisabled}
          >
            {t(opt.label)}
          </TabsTrigger>
        ))}
      </TabsList>
    </Tabs>
  )

  const chartTheme = resolvedTheme === 'dark' ? 'dark' : 'light'

  return (
    <div className='space-y-3'>
      <div className='flex items-center gap-1.5 overflow-x-auto pb-1 sm:gap-2'>
        <Tabs
          value={dateActive ? '' : String(props.selectedRange)}
          onValueChange={handleRangeChange}
          className='shrink-0'
        >
          <TabsList>
            {TIME_RANGE_PRESETS.map((preset) => (
              <TabsTrigger
                key={preset.days}
                value={String(preset.days)}
                className='px-2.5 text-xs'
              >
                {t(preset.label)}
              </TabsTrigger>
            ))}
          </TabsList>
        </Tabs>

        {props.onSelectedDateChange && (
          <DatePicker
            selected={
              props.selectedDate ? dayjs(props.selectedDate).toDate() : undefined
            }
            onSelect={handleSelectedDateSelect}
            onClear={handleSelectedDateClear}
            className='w-[160px]'
          />
        )}

        {props.granularityDisabledTitleKey ? (
          <Tooltip>
            <TooltipTrigger render={<div className='shrink-0' tabIndex={0} />}>
              {granularityTabs}
            </TooltipTrigger>
            <TooltipContent>
              {t(props.granularityDisabledTitleKey)}
            </TooltipContent>
          </Tooltip>
        ) : (
          granularityTabs
        )}

        <Tabs
          value={String(props.topLimit)}
          onValueChange={handleTopLimitChange}
          className='shrink-0'
        >
          <TabsList>
            <span className='text-muted-foreground px-2 text-xs font-medium whitespace-nowrap'>
              {t(props.topLimitLabelKey)}
            </span>
            {TOP_N_LIMIT_OPTIONS.map((limit) => (
              <TabsTrigger
                key={limit}
                value={String(limit)}
                className='px-2.5 text-xs'
              >
                {t('Top {{count}}', { count: limit })}
              </TabsTrigger>
            ))}
          </TabsList>
        </Tabs>

        {props.isLoading && (
          <Loader2 className='text-muted-foreground size-4 animate-spin' />
        )}
      </div>

      <div className='grid gap-3'>
        {props.charts.map((chart) => (
          <div
            key={chart.value}
            className='overflow-hidden rounded-lg border'
          >
            <div className='flex w-full items-center gap-2 border-b px-3 py-2 sm:px-5 sm:py-3'>
              <IconBadge tone='info' size='sm'>
                <props.icon />
              </IconBadge>
              <div className='text-sm font-semibold'>{t(chart.labelKey)}</div>
            </div>

            <div
              className={
                chart.barCount != null
                  ? 'p-1.5 sm:p-2'
                  : 'h-[300px] p-1.5 sm:h-96 sm:p-2'
              }
              style={
                chart.barCount != null
                  ? { height: computeRankChartHeight(chart.barCount) }
                  : undefined
              }
            >
              {props.isLoading ? (
                <Skeleton className='h-full w-full' />
              ) : (
                themeReady &&
                chart.spec && (
                  <VChart
                    key={`${props.chartKeyPrefix}-${chart.value}-${props.topLimit}-${resolvedTheme}`}
                    spec={{
                      ...chart.spec,
                      theme: chartTheme,
                      background: 'transparent',
                    }}
                    option={VCHART_OPTION}
                  />
                )
              )}
            </div>
          </div>
        ))}
      </div>
    </div>
  )
}
