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
import { useMemo } from 'react'

import dayjs from '@/lib/dayjs'
import { formatChartTime, getRollingDateRange } from '@/lib/time'

/**
 * Rolling unix-seconds window derived from the selected range preset.
 */
export function useTopNTimeRange(selectedRange: number) {
  return useMemo(() => {
    const { start, end } = getRollingDateRange(selectedRange)
    return {
      start_timestamp: Math.floor(start.getTime() / 1000),
      end_timestamp: Math.floor(end.getTime() / 1000),
    }
  }, [selectedRange])
}

const DATE_ONLY_PATTERN = /^\d{4}-\d{2}-\d{2}$/

/**
 * Parse a yyyy-MM-dd selection as a local-midnight dayjs instance.
 * Null when malformed, calendar-overflowing, or later than today.
 */
function parseSelectedDate(
  selectedDate: string,
  now: Date
): ReturnType<typeof dayjs> | null {
  if (!DATE_ONLY_PATTERN.test(selectedDate)) {
    return null
  }
  const parsed = dayjs(selectedDate)
  if (parsed.format('YYYY-MM-DD') !== selectedDate) {
    return null
  }
  if (parsed.startOf('day').isAfter(dayjs(now).startOf('day'))) {
    return null
  }
  return parsed
}

/**
 * Natural-day window for the selected date: a historical day spans
 * 00:00:00~23:59:59 local time, today spans 00:00:00 through `now`.
 */
export function resolveDayWindow(
  selectedDate: string,
  now: Date = new Date()
): { start_timestamp: number; end_timestamp: number } | null {
  const parsed = parseSelectedDate(selectedDate, now)
  if (!parsed) {
    return null
  }
  const start = parsed.startOf('day')
  if (start.isSame(dayjs(now).startOf('day'))) {
    return {
      start_timestamp: start.unix(),
      end_timestamp: Math.floor(now.getTime() / 1000),
    }
  }
  return {
    start_timestamp: start.unix(),
    end_timestamp: parsed.endOf('day').unix(),
  }
}

/**
 * 24 hourly axis labels (MM-DD HH:00) for the selected natural day.
 */
export function buildDayHourPoints(selectedDate: string): string[] {
  const parsed = parseSelectedDate(selectedDate, new Date())
  if (!parsed) {
    return []
  }
  const startSec = parsed.startOf('day').unix()
  return Array.from({ length: 24 }, (_, hour) =>
    formatChartTime(startSec + hour * 3600, 'hour')
  )
}
