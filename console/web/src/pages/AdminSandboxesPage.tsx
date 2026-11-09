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
      message.error(error instanceof Error ? error.message : 'Failed to load sandboxes');
    } finally {
      if (seq === requestSeq.current) setLoading(false);
    }
  }, [stateFilter, tenantFilter]);

  useEffect(() => {
    void load();
  }, [load]);

  const columns: ColumnsType<Sandbox> = [
    {
      title: 'Tenant',
      dataIndex: 'tenant',
      width: 128,
      ellipsis: true,
      onCell: () => consoleTagTableCellProps,
      render: (t: string | undefined) => <TenantTag tenant={t} />,
    },
    {
      title: 'Name / ID',
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
      title: 'Status',
      width: 124,
      ellipsis: true,
      onCell: () => consoleTagTableCellProps,
      render: (_, r) => <SandboxStateTag state={r.status?.state} />,
    },
    {
      title: 'Uptime',
      render: (_, r) => formatDuration(r.runtimeSummary?.wallClockSeconds),
    },
    {
      title: 'Remaining',
      render: (_, r) =>
        r.runtimeSummary?.remainingSeconds !== undefined
          ? formatDuration(r.runtimeSummary.remainingSeconds)
          : '—',
    },
    {
      title: 'Actions',
      render: (_, r) => (
        <Space>
          <Link to={`/sandboxes/${r.id}?tenant=${encodeURIComponent(r.tenant ?? '')}&tab=logs`}>
            Logs
          </Link>
          <Link
            to={`/diagnostics?sandboxId=${encodeURIComponent(r.id)}&tenant=${encodeURIComponent(r.tenant ?? '')}`}
          >
            Diagnostics
          </Link>
          <ShellLink sandboxId={r.id} state={r.status?.state} tenant={r.tenant} />
        </Space>
      ),
    },
  ];

  return (
    <div>
      <Typography.Title level={4}>Sandboxes (All Tenants)</Typography.Title>
      <Typography.Paragraph type="secondary">
        Current sandbox list from Lifecycle; these are not persisted request records. Admins see all tenants by default and can filter by tenant.
      </Typography.Paragraph>
      {capped && (
        <Alert
          type="info"
          showIcon
          style={{ marginBottom: 16 }}
          message={`Showing up to ${ADMIN_LIST_PAGE_SIZE} sandboxes per tenant. Additional sandboxes are not listed.`}
        />
      )}
      {tenantErrors.length > 0 && (
        <Alert
          type="warning"
          showIcon
          style={{ marginBottom: 16 }}
          message="Some tenant upstream requests failed"
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
          placeholder="Status"
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
