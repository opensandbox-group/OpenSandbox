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

import { Alert, Select, Space, Table, Typography } from 'antd';
import type { ColumnsType } from 'antd/es/table';
import { useEffect, useState } from 'react';
import { Link } from 'react-router-dom';

import { adminApi } from '../api/client';
import { AdminTenantSelect, ALL_TENANTS, tenantFilterToQuery } from '../components/AdminTenantSelect';
import type { Sandbox } from '../api/types';
import { consoleTagTableCellProps, SandboxStateTag, TenantTag } from '../components/SemanticTags';
import { formatDuration, SANDBOX_STATES, sandboxDisplayName } from '../utils/format';

export function AdminSandboxesPage() {
  const [loading, setLoading] = useState(false);
  const [stateFilter, setStateFilter] = useState<string | undefined>();
  const [tenantFilter, setTenantFilter] = useState(ALL_TENANTS);
  const [items, setItems] = useState<Sandbox[]>([]);
  const [tenantErrors, setTenantErrors] = useState<
    { tenant: string; code: string; message: string }[]
  >([]);

  const load = async () => {
    setLoading(true);
    try {
      const data = await adminApi.sandboxes({
        pageSize: 200,
        state: stateFilter,
        tenant: tenantFilterToQuery(tenantFilter),
      });
      setItems(data.items ?? []);
      setTenantErrors(data.pagination?.tenantErrors ?? []);
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    void load();
  }, [stateFilter, tenantFilter]);

  const columns: ColumnsType<Sandbox> = [
    {
      title: '租户',
      dataIndex: 'tenant',
      width: 128,
      ellipsis: true,
      onCell: () => consoleTagTableCellProps,
      render: (t: string | undefined) => <TenantTag tenant={t} />,
    },
    {
      title: '名称 / ID',
      render: (_, r) => (
        <Link to={`/sandboxes/${r.id}?tenant=${encodeURIComponent(r.tenant ?? '')}`}>
          {sandboxDisplayName(r)}
        </Link>
      ),
    },
    {
      title: 'Namespace',
      dataIndex: 'namespace',
    },
    {
      title: '状态',
      width: 124,
      ellipsis: true,
      onCell: () => consoleTagTableCellProps,
      render: (_, r) => <SandboxStateTag state={r.status?.state} />,
    },
    {
      title: '运行时长',
      render: (_, r) => formatDuration(r.runtimeSummary?.wallClockSeconds),
    },
    {
      title: '剩余',
      render: (_, r) =>
        r.runtimeSummary?.remainingSeconds !== undefined
          ? formatDuration(r.runtimeSummary.remainingSeconds)
          : '—',
    },
    {
      title: '操作',
      render: (_, r) => (
        <Space>
          <Link to={`/sandboxes/${r.id}?tenant=${encodeURIComponent(r.tenant ?? '')}&tab=logs`}>
            日志
          </Link>
          <Link
            to={`/diagnostics?sandboxId=${encodeURIComponent(r.id)}&tenant=${encodeURIComponent(r.tenant ?? '')}`}
          >
            诊断
          </Link>
        </Space>
      ),
    },
  ];

  return (
    <div>
      <Typography.Title level={4}>沙箱（全局）</Typography.Title>
      <Typography.Paragraph type="secondary">
        来自 Lifecycle 的当前沙箱列表。持久化申请记录见侧栏「沙箱 → 申请历史」。Admin 默认全部租户，可筛选单租户。
      </Typography.Paragraph>
      {tenantErrors.length > 0 && (
        <Alert
          type="warning"
          showIcon
          style={{ marginBottom: 16 }}
          message="部分租户 upstream 失败"
          description={
            <ul style={{ margin: 0, paddingLeft: 20 }}>
              {tenantErrors.map((e) => (
                <li key={e.tenant}>
                  {e.tenant}: {e.code} — {e.message}
                </li>
              ))}
            </ul>
          }
        />
      )}
      <Space style={{ marginBottom: 16 }}>
        <AdminTenantSelect value={tenantFilter} onChange={setTenantFilter} />
        <Select
          allowClear
          placeholder="状态"
          style={{ width: 160 }}
          value={stateFilter}
          onChange={setStateFilter}
          options={SANDBOX_STATES.map((s) => ({ label: s, value: s }))}
        />
      </Space>
      <Table rowKey="id" loading={loading} columns={columns} dataSource={items} pagination={false} />
    </div>
  );
}
