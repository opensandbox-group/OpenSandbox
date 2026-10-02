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

import { Alert, Card, Col, Row, Space, Statistic, Table, Typography } from 'antd';
import type { ColumnsType } from 'antd/es/table';
import type { Dayjs } from 'dayjs';
import { useEffect, useMemo, useRef, useState } from 'react';
import { Link, useSearchParams } from 'react-router-dom';

import { historyApi } from '../api/client';
import { AdminTenantSelect, ALL_TENANTS, tenantFilterToQuery } from '../components/AdminTenantSelect';
import { consoleTagTableCellProps, SandboxStateTag, TenantTag } from '../components/SemanticTags';
import { UsagePeriodFilterBar } from '../components/UsagePeriodFilterBar';
import type { SandboxHistoryItem, SandboxHistoryStats } from '../api/types';
import { useAuth } from '../auth/AuthContext';
import { formatDuration } from '../utils/format';
import { currentMonthRangeUtc, monthRangeToUsageQuery, usageQueryToDayRange } from '../utils/usagePeriod';

export function SandboxHistoryPage() {
  const { user } = useAuth();
  const isAdmin = user?.role === 'admin';
  const [searchParams, setSearchParams] = useSearchParams();
  const periodFrom = searchParams.get('from') ?? undefined;
  const periodTo = searchParams.get('to') ?? undefined;
  const periodActive = Boolean(periodFrom && periodTo);
  const urlTenant = searchParams.get('tenant');
  const defaultedPeriod = useRef(false);

  const [loading, setLoading] = useState(false);
  const [items, setItems] = useState<SandboxHistoryItem[]>([]);
  const [stats, setStats] = useState<SandboxHistoryStats | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [tenantFilter, setTenantFilter] = useState(ALL_TENANTS);
  const [page, setPage] = useState(1);
  const [totalItems, setTotalItems] = useState(0);

  useEffect(() => {
    if (defaultedPeriod.current) return;
    defaultedPeriod.current = true;
    if (!periodFrom && !periodTo) {
      const next = new URLSearchParams(searchParams);
      const q = monthRangeToUsageQuery(currentMonthRangeUtc());
      next.set('from', q.from);
      next.set('to', q.to);
      setSearchParams(next, { replace: true });
    }
  }, [periodFrom, periodTo, searchParams, setSearchParams]);

  useEffect(() => {
    if (isAdmin && urlTenant) {
      setTenantFilter(urlTenant);
    }
  }, [isAdmin, urlTenant]);

  useEffect(() => {
    setPage(1);
  }, [tenantFilter, periodFrom, periodTo]);

  useEffect(() => {
    void (async () => {
      setLoading(true);
      setError(null);
      try {
        const tenant = isAdmin ? tenantFilterToQuery(tenantFilter) : undefined;
        const periodQuery =
          periodFrom && periodTo ? { from: periodFrom, to: periodTo } : undefined;
        const [list, st] = await Promise.all([
          historyApi.listSandboxes({
            page,
            pageSize: 20,
            ...(tenant ? { tenant } : {}),
            ...periodQuery,
          }),
          periodActive ? Promise.resolve({ enabled: true } as SandboxHistoryStats) : historyApi.stats(tenant),
        ]);
        setItems(list.items ?? []);
        setTotalItems(list.pagination?.totalItems ?? list.items?.length ?? 0);
        setStats(periodActive ? null : st);
      } catch (e) {
        setError(e instanceof Error ? e.message : '加载历史失败');
      } finally {
        setLoading(false);
      }
    })();
  }, [isAdmin, tenantFilter, page, periodFrom, periodTo, periodActive]);

  const dayRange = useMemo((): [Dayjs, Dayjs] | null => {
    if (!periodFrom || !periodTo) return null;
    return usageQueryToDayRange(periodFrom, periodTo);
  }, [periodFrom, periodTo]);

  const syncTenantToUrl = (nextTenant: string) => {
    setTenantFilter(nextTenant);
    if (!isAdmin) return;
    const next = new URLSearchParams(searchParams);
    const q = tenantFilterToQuery(nextTenant);
    if (q) next.set('tenant', q);
    else next.delete('tenant');
    setSearchParams(next, { replace: true });
  };

  const applyDayRange = (range: [Dayjs, Dayjs]) => {
    const q = monthRangeToUsageQuery(range);
    const next = new URLSearchParams(searchParams);
    next.set('from', q.from);
    next.set('to', q.to);
    setSearchParams(next, { replace: true });
  };

  const clearPeriodFilter = () => {
    const next = new URLSearchParams(searchParams);
    next.delete('from');
    next.delete('to');
    setSearchParams(next, { replace: true });
  };

  const onPeriodRangeChange = (range: [Dayjs, Dayjs] | null) => {
    if (!range) {
      clearPeriodFilter();
      return;
    }
    applyDayRange(range);
  };

  const columns: ColumnsType<SandboxHistoryItem> = [
    { title: '沙箱 ID', dataIndex: 'sandboxId', ellipsis: true },
    ...(isAdmin
      ? [
          {
            title: '租户',
            dataIndex: 'tenant',
            width: 128,
            ellipsis: true,
            onCell: () => consoleTagTableCellProps,
            render: (t: string) => <TenantTag tenant={t} />,
          },
        ]
      : []),
    {
      title: '状态',
      dataIndex: 'state',
      width: 124,
      ellipsis: true,
      onCell: () => consoleTagTableCellProps,
      render: (s) => <SandboxStateTag state={s} />,
    },
    {
      title: '生命周期时长',
      dataIndex: 'wallClockSeconds',
      width: 120,
      render: (v: number | undefined) => (v != null ? formatDuration(v) : '—'),
    },
    { title: '创建时间', dataIndex: 'createdAt', width: 180 },
    { title: '结束/删除', dataIndex: 'endedAt', width: 180, render: (_, r) => r.endedAt ?? r.deletedAt ?? '—' },
    {
      title: '快照数',
      dataIndex: 'snapshotCount',
      width: 80,
      render: (n: number | undefined) => n ?? 0,
    },
    {
      title: '操作',
      key: 'actions',
      render: (_, r) => (
        <Link
          to={
            isAdmin
              ? `/sandboxes/${r.sandboxId}?tenant=${encodeURIComponent(r.tenant ?? '')}`
              : `/sandboxes/${r.sandboxId}`
          }
        >
          详情
        </Link>
      ),
    },
  ];

  return (
    <div>
      <Typography.Title level={4}>沙箱 · 申请历史</Typography.Title>
      <Typography.Paragraph type="secondary">
        PostgreSQL 表 <code>sandbox_lifecycle_history</code>（Server 生命周期审计 + Console 扩展）。需 Server 开启{' '}
        <code>[store.lifecycle_audit]</code> 后 SDK 直连也会入库。
      </Typography.Paragraph>

      <Card size="small" style={{ marginBottom: 16 }}>
        <Space wrap align="center">
          {isAdmin && <AdminTenantSelect value={tenantFilter} onChange={syncTenantToUrl} width={220} />}
          <UsagePeriodFilterBar range={dayRange} onRangeChange={onPeriodRangeChange} allowAllTime />
        </Space>
        {periodActive ? (
          <Typography.Text type="secondary" style={{ display: 'block', marginTop: 8 }}>
            筛选区间（UTC，与用量分摊「沙箱数」同口径：生命周期与区间有重叠即计入）：{' '}
            <code>{periodFrom}</code> — <code>{periodTo}</code>
            {totalItems >= 0 ? ` · 共 ${totalItems} 条` : null}
          </Typography.Text>
        ) : (
          <Typography.Text type="secondary" style={{ display: 'block', marginTop: 8 }}>
            未限定时间，展示全部历史记录。
          </Typography.Text>
        )}
      </Card>

      {error && <Alert type="error" message={error} style={{ marginBottom: 16 }} />}
      {!periodActive && stats?.enabled && (
        <Row gutter={16} style={{ marginBottom: 16 }}>
          <Col span={6}>
            <Card size="small">
              <Statistic title="历史条数" value={stats.totalRecords ?? 0} />
            </Card>
          </Col>
          <Col span={6}>
            <Card size="small">
              <Statistic title="仍活跃（未删/未终态）" value={stats.activeRecords ?? 0} />
            </Card>
          </Col>
          <Col span={6}>
            <Card size="small">
              <Statistic title="累计运行时长" value={formatDuration(stats.totalWallClockSeconds ?? 0)} />
            </Card>
          </Col>
          <Col span={6}>
            <Card size="small">
              <Statistic title="平均运行时长" value={formatDuration(stats.avgWallClockSeconds ?? 0)} />
            </Card>
          </Col>
        </Row>
      )}
      <Table
        rowKey="sandboxId"
        loading={loading}
        columns={columns}
        dataSource={items}
        tableLayout="fixed"
        scroll={{ x: 'max-content' }}
        pagination={{
          current: page,
          pageSize: 20,
          total: totalItems,
          onChange: (p) => setPage(p),
          showTotal: (t) => `共 ${t} 条`,
        }}
      />
    </div>
  );
}
