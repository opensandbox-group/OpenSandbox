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

import { Alert, Select, Space, Table, Typography, message } from 'antd';
import type { ColumnsType } from 'antd/es/table';
import { useCallback, useEffect, useRef, useState } from 'react';
import { Link } from 'react-router-dom';

import { adminApi } from '../api/client';
import { AdminTenantSelect, ALL_TENANTS, tenantFilterToQuery } from '../components/AdminTenantSelect';
import type { Sandbox } from '../api/types';
import { consoleTagTableCellProps, SandboxStateTag, TenantTag } from '../components/SemanticTags';
import { formatDuration, SANDBOX_STATES, sandboxDisplayName } from '../utils/format';
import { ShellLink } from '../components/ShellLink';

/** Lifecycle maximum page size. A full page can hide later sandboxes for that tenant. */
const ADMIN_LIST_PAGE_SIZE = 200;

export function AdminSandboxesPage() {
  const [loading, setLoading] = useState(false);
  const [stateFilter, setStateFilter] = useState<string | undefined>();
  const [tenantFilter, setTenantFilter] = useState(ALL_TENANTS);
  const [items, setItems] = useState<Sandbox[]>([]);
  const [tenantErrors, setTenantErrors] = useState<
    { tenant: string; code: string; message: string }[]
  >([]);
  const [capped, setCapped] = useState(false);
  const requestSeq = useRef(0);

  const load = useCallback(async () => {
    const seq = ++requestSeq.current;
    setLoading(true);
    try {
      const data = await adminApi.sandboxes({
        pageSize: ADMIN_LIST_PAGE_SIZE,
        state: stateFilter,
        tenant: tenantFilterToQuery(tenantFilter),
      });
      if (seq !== requestSeq.current) return;
      setItems(data.items ?? []);
      setTenantErrors(data.pagination?.tenantErrors ?? []);
      setCapped(Boolean(data.pagination?.capped));
    } catch (error) {
      if (seq !== requestSeq.current) return;
      setItems([]);
      setTenantErrors([]);
      setCapped(false);
      message.error(error instanceof Error ? error.message : '加载沙箱失败');
    } finally {
      if (seq === requestSeq.current) setLoading(false);
    }
  }, [stateFilter, tenantFilter]);

  useEffect(() => {
    void load();
  }, [load]);

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
          <ShellLink sandboxId={r.id} state={r.status?.state} tenant={r.tenant} />
        </Space>
      ),
    },
  ];

  return (
    <div>
      <Typography.Title level={4}>沙箱（全局）</Typography.Title>
      <Typography.Paragraph type="secondary">
        来自 Lifecycle 的当前沙箱列表，不是持久化申请记录。Admin 默认全部租户，可筛选单租户。
      </Typography.Paragraph>
      {capped && (
        <Alert
          type="info"
          showIcon
          style={{ marginBottom: 16 }}
          message={`每个租户最多显示 ${ADMIN_LIST_PAGE_SIZE} 条，超出的沙箱未列出。`}
        />
      )}
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
