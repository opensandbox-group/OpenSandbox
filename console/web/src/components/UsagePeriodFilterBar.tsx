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

import { DatePicker, Select, Space } from 'antd';
import dayjs, { type Dayjs } from 'dayjs';
import utc from 'dayjs/plugin/utc';
import type { ReactNode } from 'react';
import { useMemo } from 'react';

import {
  matchUsagePeriodPreset,
  USAGE_PERIOD_PRESET_LABELS,
  usagePeriodPresetRange,
  type UsagePeriodPreset,
} from '../utils/usagePeriod';

dayjs.extend(utc);

type PresetSelectValue = Exclude<UsagePeriodPreset, 'custom'>;

function buildSelectOptions(allowAllTime: boolean) {
  const keys: PresetSelectValue[] = allowAllTime
    ? ['all', 'thisMonth', 'lastMonth', 'thisQuarter', 'lastQuarter']
    : ['thisMonth', 'lastMonth', 'thisQuarter', 'lastQuarter'];
  return [
    ...keys.map((value) => ({
      value,
      label: USAGE_PERIOD_PRESET_LABELS[value],
    })),
    { value: 'custom' as const, label: USAGE_PERIOD_PRESET_LABELS.custom, disabled: true },
  ];
}

export function UsagePeriodFilterBar({
  range,
  onRangeChange,
  allowAllTime = false,
  trailing,
}: {
  /** Inclusive UTC day range; `null` means all time when {@link allowAllTime} is true. */
  range: [Dayjs, Dayjs] | null;
  onRangeChange: (range: [Dayjs, Dayjs] | null) => void;
  allowAllTime?: boolean;
  trailing?: ReactNode;
}) {
  const matched = useMemo(
    () => matchUsagePeriodPreset(range, { allowAll: allowAllTime }),
    [allowAllTime, range],
  );

  const selectOptions = useMemo(() => buildSelectOptions(allowAllTime), [allowAllTime]);

  const onPresetSelect = (key: UsagePeriodPreset) => {
    if (key === 'custom') return;
    if (key === 'all') {
      onRangeChange(null);
      return;
    }
    onRangeChange(usagePeriodPresetRange(key));
  };

  return (
    <Space wrap align="center">
      <Select<UsagePeriodPreset>
        value={matched}
        style={{ width: 120 }}
        options={selectOptions}
        onChange={onPresetSelect}
      />
      <DatePicker.RangePicker
        value={range}
        placeholder={['开始日期', '结束日期']}
        onChange={(v) => {
          if (v?.[0] && v[1]) {
            onRangeChange([
              dayjs.utc(v[0].format('YYYY-MM-DD')),
              dayjs.utc(v[1].format('YYYY-MM-DD')),
            ]);
          } else if (allowAllTime && !v) {
            onRangeChange(null);
          }
        }}
      />
      {trailing}
    </Space>
  );
}
