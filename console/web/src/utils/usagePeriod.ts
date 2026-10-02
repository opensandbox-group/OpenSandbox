// Copyright 2026 The OpenSandbox Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

import dayjs, { type Dayjs } from 'dayjs';
import utc from 'dayjs/plugin/utc';

dayjs.extend(utc);

export function toIsoUtcStart(d: Dayjs): string {
  return d.utc().startOf('day').toISOString().replace('.000Z', 'Z');
}

export function toIsoUtcEndExclusive(d: Dayjs): string {
  return d.utc().add(1, 'day').startOf('day').toISOString().replace('.000Z', 'Z');
}

export function currentMonthRangeUtc(): [Dayjs, Dayjs] {
  const start = dayjs.utc().startOf('month');
  const end = dayjs.utc().endOf('month');
  return [start, end];
}

export function previousMonthRangeUtc(): [Dayjs, Dayjs] {
  const anchor = dayjs.utc().subtract(1, 'month');
  return [anchor.startOf('month'), anchor.endOf('month')];
}

/** Same as {@link currentMonthRangeUtc}; named for preset buttons. */
export function presetThisMonth(): [Dayjs, Dayjs] {
  return currentMonthRangeUtc();
}

export function presetLastMonth(): [Dayjs, Dayjs] {
  return previousMonthRangeUtc();
}

export function presetThisQuarter(): [Dayjs, Dayjs] {
  const now = dayjs.utc();
  const q = Math.floor(now.month() / 3);
  const start = dayjs.utc().month(q * 3).startOf('month');
  const end = start.add(3, 'month').subtract(1, 'day');
  return [start, end];
}

export function presetLastQuarter(): [Dayjs, Dayjs] {
  const now = dayjs.utc();
  const q = Math.floor(now.month() / 3) - 1;
  const year = q < 0 ? now.year() - 1 : now.year();
  const month = q < 0 ? 9 : q * 3;
  const start = dayjs.utc().year(year).month(month).startOf('month');
  const end = start.add(3, 'month').subtract(1, 'day');
  return [start, end];
}

export function monthRangeToUsageQuery(range: [Dayjs, Dayjs]) {
  return { from: toIsoUtcStart(range[0]), to: toIsoUtcEndExclusive(range[1]) };
}

/** Parse usage-style period query back to inclusive UTC day range for DatePicker. */
export function usageQueryToDayRange(from: string, to: string): [Dayjs, Dayjs] {
  const start = dayjs.utc(from);
  const endExclusive = dayjs.utc(to);
  const endInclusive = endExclusive.subtract(1, 'millisecond').startOf('day');
  return [start.startOf('day'), endInclusive];
}

export type UsagePeriodPreset =
  | 'all'
  | 'thisMonth'
  | 'lastMonth'
  | 'thisQuarter'
  | 'lastQuarter'
  | 'custom';

function rangesSameUtcDay(a: [Dayjs, Dayjs], b: [Dayjs, Dayjs]): boolean {
  return a[0].utc().isSame(b[0].utc(), 'day') && a[1].utc().isSame(b[1].utc(), 'day');
}

export function usagePeriodPresetRange(key: Exclude<UsagePeriodPreset, 'custom' | 'all'>): [Dayjs, Dayjs] {
  switch (key) {
    case 'thisMonth':
      return presetThisMonth();
    case 'lastMonth':
      return presetLastMonth();
    case 'thisQuarter':
      return presetThisQuarter();
    case 'lastQuarter':
      return presetLastQuarter();
  }
}

export function matchUsagePeriodPreset(
  range: [Dayjs, Dayjs] | null | undefined,
  opts?: { allowAll?: boolean },
): UsagePeriodPreset {
  if (!range) {
    return opts?.allowAll ? 'all' : 'custom';
  }
  const presets: Exclude<UsagePeriodPreset, 'custom' | 'all'>[] = [
    'thisMonth',
    'lastMonth',
    'thisQuarter',
    'lastQuarter',
  ];
  for (const key of presets) {
    if (rangesSameUtcDay(range, usagePeriodPresetRange(key))) {
      return key;
    }
  }
  return 'custom';
}

export const USAGE_PERIOD_PRESET_LABELS: Record<UsagePeriodPreset, string> = {
  all: '全部时间',
  thisMonth: '本月',
  lastMonth: '上月',
  thisQuarter: '本季度',
  lastQuarter: '上季度',
  custom: '自定义',
};
