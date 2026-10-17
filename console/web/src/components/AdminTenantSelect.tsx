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

import { Select } from 'antd';
import { useEffect, useMemo, useState } from 'react';

import { adminApi } from '../api/client';

/** Select value meaning no tenant filter (all tenants). */
export const ALL_TENANTS = '__all__';

export function tenantFilterToQuery(value: string): string | undefined {
  return value === ALL_TENANTS ? undefined : value;
}

type Props = {
  value: string;
  onChange: (value: string) => void;
  width?: number;
  placeholder?: string;
};

export function AdminTenantSelect({ value, onChange, width = 240, placeholder }: Props) {
  const [tenantOptions, setTenantOptions] = useState<{ label: string; value: string }[]>([]);

  useEffect(() => {
    void (async () => {
      try {
        const data = await adminApi.listTenants();
        setTenantOptions(
          (data.items ?? []).map((t) => ({
            label: t.namespace ? `${t.name} (${t.namespace})` : t.name,
            value: t.name,
          })),
        );
      } catch {
        setTenantOptions([]);
      }
    })();
  }, []);

  const options = useMemo(
    () => [{ label: '全部租户', value: ALL_TENANTS }, ...tenantOptions],
    [tenantOptions],
  );

  return (
    <Select
      showSearch
      style={{ width }}
      value={value}
      onChange={onChange}
      options={options}
      optionFilterProp="label"
      placeholder={placeholder ?? '全部租户'}
    />
  );
}
