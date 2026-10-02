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

import { Alert, Button, Card, Col, DatePicker, Row, Space, Statistic, Table, Typography } from 'antd';
import type { ColumnsType } from 'antd/es/table';
import dayjs, { type Dayjs } from 'dayjs';
import utc from 'dayjs/plugin/utc';
import { useCallback, useEffect, useMemo, useState } from 'react';
import { Link } from 'react-router-dom';

import { ApiError, historyApi } from '../api/client';
import { AdminTenantSelect, ALL_TENANTS, tenantFilterToQuery } from '../components/AdminTenantSelect';
import type { ImageStatsRow } from '../api/types';
import { useAuth } from '../auth/AuthContext';
import { currentMonthRangeUtc, monthRangeToUsageQuery } from '../utils/usagePeriod';

dayjs.extend(utc);

type RangeMode = 'all' | 'month';

export function SandboxImageStatsPage() {
  const { user } = useAuth();
  const isAdmin = user?.role === 'admin';
  const [tenantFilter, setTenantFilter] = useState(ALL_TENANTS);
  const [rangeMode, setRangeMode] = useState<RangeMode>('all');
  const [monthRange, setMonthRange] = useState<[Dayjs, Dayjs]>(() => {
    const [s, e] = currentMonthRangeUtc();
    return [s, e];
  });
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [historyOff, setHistoryOff] = useState(false);
  const [items, setItems] = useState<ImageStatsRow[]>([]);
  const [totals, setTotals] = useState({ totalRecords: 0, distinctImages: 0 });

  const load = useCallback(async () => {
    setLoading(true);
    setError(null);
    setHistoryOff(false);
    try {
      const tenant = isAdmin ? tenantFilterToQuery(tenantFilter) : undefined;
      const query: { tenant?: string; from?: string; to?: string } = {};
      if (tenant) query.tenant = tenant;
      if (rangeMode === 'month') {
        Object.assign(query, monthRangeToUsageQuery(monthRange));
      }
      const resp = await historyApi.imageStats(query);
      if (resp.enabled === false) {
        setHistoryOff(true);
        setItems([]);
        return;
      }
      setItems(resp.items ?? []);
      setTotals({
        totalRecords: resp.totalRecords ?? 0,
        distinctImages: resp.distinctImages ?? 0,
      });
    } catch (e) {
      if (e instanceof ApiError && e.status === 503) {
        setHistoryOff(true);
        return;
      }
      setError(e instanceof Error ? e.message : '加载失败');
    } finally {
      setLoading(false);
    }
  }, [isAdmin, monthRange, rangeMode, tenantFilter]);

  useEffect(() => {
    void load();
  }, [load]);

  const columns: ColumnsType<ImageStatsRow> = useMemo(
    () => [
      {
        title: '基础镜像',
        dataIndex: 'imageUri',
        ellipsis: true,
        render: (uri: string) => (
          <Typography.Text copyable={{ text: uri }} style={{ fontSize: 13 }}>
            {uri}
          </Typography.Text>
        ),
      },
      {
        title: '申请次数',
        dataIndex: 'sandboxCount',
        width: 100,
        sorter: (a, b) => a.sandboxCount - b.sandboxCount,
        defaultSortOrder: 'descend',
      },
      {
        title: '占比',
        dataIndex: 'sharePercent',
        width: 88,
        render: (v: number) => `${v}%`,
      },
      {
        title: '进行中',
        dataIndex: 'activeCount',
        width: 88,
      },
      ...(isAdmin && !tenantFilterToQuery(tenantFilter)
        ? [
            {
              title: '租户数',
              dataIndex: 'tenantCount',
              width: 88,
            } as ColumnsType<ImageStatsRow>[number],
          ]
        : []),
      {
        title: '最近使用',
        dataIndex: 'lastUsedAt',
        width: 200,
        render: (v: string | undefined) => v ?? '—',
      },
    ],
    [isAdmin, tenantFilter],
  );

  return (
    <div>
      <Space style={{ width: '100%', justifyContent: 'space-between', marginBottom: 16 }} align="start">
        <div>
          <Typography.Title level={4} style={{ margin: 0 }}>
            镜像统计
          </Typography.Title>
          <Typography.Paragraph type="secondary" style={{ marginBottom: 0, marginTop: 4 }}>
            按 Console 历史库中的 <code>image_uri</code> 汇总，反映各租户曾使用的沙箱基础镜像（与实时 Lifecycle 列表互补）。
          </Typography.Paragraph>
        </div>
        {isAdmin && <AdminTenantSelect value={tenantFilter} onChange={setTenantFilter} width={220} />}
      </Space>

      {historyOff && (
        <Alert
          type="warning"
          showIcon
          style={{ marginBottom: 16 }}
          message="历史统计未启用"
          description="需配置 BFF_HISTORY_ENABLED 与 BFF_HISTORY_DATABASE_URL。"
        />
      )}
      {error && <Alert type="error" message={error} style={{ marginBottom: 16 }} />}

      <Card style={{ marginBottom: 16 }}>
        <Space wrap>
          <Button type={rangeMode === 'all' ? 'primary' : 'default'} onClick={() => setRangeMode('all')}>
            全部时间
          </Button>
          <Button type={rangeMode === 'month' ? 'primary' : 'default'} onClick={() => setRangeMode('month')}>
            按月份
          </Button>
          {rangeMode === 'month' && (
            <DatePicker.RangePicker
              picker="month"
              value={monthRange}
              onChange={(v) => {
                if (v?.[0] && v[1]) {
                  setMonthRange([v[0].utc().startOf('month'), v[1].utc().endOf('month')]);
                }
              }}
            />
          )}
          <Button onClick={() => void load()}>刷新</Button>
          <Link to="/history/sandboxes">查看申请历史 →</Link>
        </Space>
      </Card>

      <Row gutter={16} style={{ marginBottom: 16 }}>
        <Col xs={12} sm={8}>
          <Card>
            <Statistic title="历史记录数" value={totals.totalRecords} loading={loading} />
          </Card>
        </Col>
        <Col xs={12} sm={8}>
          <Card>
            <Statistic title="不同镜像数" value={totals.distinctImages} loading={loading} />
          </Card>
        </Col>
      </Row>

      <Table<ImageStatsRow>
        rowKey="imageUri"
        loading={loading}
        columns={columns}
        dataSource={items}
        pagination={{ pageSize: 20, showSizeChanger: true }}
      />
    </div>
  );
}
