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

import { QuestionCircleOutlined } from '@ant-design/icons';
import { Alert, Button, Card, Col, Row, Space, Statistic, Table, Typography } from 'antd';
import type { ColumnsType } from 'antd/es/table';
import dayjs, { type Dayjs } from 'dayjs';
import utc from 'dayjs/plugin/utc';
import { useCallback, useEffect, useMemo, useState } from 'react';
import { Link } from 'react-router-dom';

import { historyApi } from '../api/client';
import { AdminTenantSelect, ALL_TENANTS, tenantFilterToQuery } from '../components/AdminTenantSelect';
import { TenantTag } from '../components/SemanticTags';
import { UsagePeriodFilterBar } from '../components/UsagePeriodFilterBar';
import { USAGE_METRIC_HINTS, UsageMetricTitle } from '../components/UsageMetricTitle';
import type { TenantUsageResponse, TenantUsageRow } from '../api/types';
import { useAuth } from '../auth/AuthContext';
import { formatDuration } from '../utils/format';
import { historySandboxesPath } from '../utils/historyLinks';
import { monthRangeToUsageQuery, presetThisMonth } from '../utils/usagePeriod';

dayjs.extend(utc);

export function TenantUsagePage() {
  const { user } = useAuth();
  const isAdmin = user?.role === 'admin';
  const [range, setRange] = useState<[Dayjs, Dayjs]>(() => presetThisMonth());
  const [loading, setLoading] = useState(false);
  const [data, setData] = useState<TenantUsageResponse | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [tenantFilter, setTenantFilter] = useState(ALL_TENANTS);

  const load = useCallback(
    async (r: [Dayjs, Dayjs]) => {
      setLoading(true);
      setError(null);
      try {
        const tenant = isAdmin ? tenantFilterToQuery(tenantFilter) : undefined;
        const resp = await historyApi.usage({
          ...monthRangeToUsageQuery(r),
          ...(tenant ? { tenant } : {}),
        });
        setData(resp);
      } catch (e) {
        setData(null);
        setError(e instanceof Error ? e.message : '加载用量失败');
      } finally {
        setLoading(false);
      }
    },
    [isAdmin, tenantFilter],
  );

  useEffect(() => {
    void load(range);
  }, [load, range]);

  const usagePeriod = data?.period;

  const columns: ColumnsType<TenantUsageRow> = useMemo(
    () => [
      ...(isAdmin
        ? [{ title: '租户', dataIndex: 'tenant', width: 120, render: (t: string) => <TenantTag tenant={t} /> }]
        : []),
      {
        title: <UsageMetricTitle label="沙箱数" hint={USAGE_METRIC_HINTS.sandboxCount} />,
        dataIndex: 'sandboxCount',
        width: 100,
        render: (v: number, row) => {
          if (!v || !usagePeriod?.from || !usagePeriod?.to) {
            return v;
          }
          return (
            <Link
              to={historySandboxesPath({
                from: usagePeriod.from,
                to: usagePeriod.to,
                tenant: isAdmin ? row.tenant : undefined,
              })}
            >
              {v}
            </Link>
          );
        },
      },
      {
        title: (
          <UsageMetricTitle label="区间内生命周期占用" hint={USAGE_METRIC_HINTS.overlapSeconds} />
        ),
        dataIndex: 'overlapSeconds',
        width: 140,
        render: (v: number) => formatDuration(v),
      },
      {
        title: <UsageMetricTitle label="CPU·秒" hint={USAGE_METRIC_HINTS.cpuCoreSeconds} />,
        dataIndex: 'cpuCoreSeconds',
        width: 120,
        render: (v: number) => v.toLocaleString(undefined, { maximumFractionDigits: 1 }),
      },
      {
        title: <UsageMetricTitle label="内存·Gi·秒" hint={USAGE_METRIC_HINTS.memoryGiSeconds} />,
        dataIndex: 'memoryGiSeconds',
        width: 130,
        render: (v: number) => v.toLocaleString(undefined, { maximumFractionDigits: 1 }),
      },
      {
        title: <UsageMetricTitle label="时长占比" hint={USAGE_METRIC_HINTS.shareTime} />,
        key: 'shareTime',
        width: 100,
        render: (_, r) => `${r.sharePercent?.time ?? 0}%`,
      },
      {
        title: <UsageMetricTitle label="CPU 占比" hint={USAGE_METRIC_HINTS.shareCpu} />,
        key: 'shareCpu',
        width: 100,
        render: (_, r) => `${r.sharePercent?.cpu ?? 0}%`,
      },
      {
        title: <UsageMetricTitle label="内存占比" hint={USAGE_METRIC_HINTS.shareMemory} />,
        key: 'shareMem',
        width: 100,
        render: (_, r) => `${r.sharePercent?.memory ?? 0}%`,
      },
      {
        title: (
          <UsageMetricTitle label="综合分摊占比" hint={USAGE_METRIC_HINTS.shareComposite} />
        ),
        key: 'shareComposite',
        width: 120,
        fixed: 'right' as const,
        render: (_, r) => (
          <Typography.Text strong>{r.sharePercent?.composite ?? 0}%</Typography.Text>
        ),
      },
    ],
    [isAdmin, usagePeriod?.from, usagePeriod?.to],
  );

  return (
    <div>
      <Typography.Title level={4} style={{ marginTop: 0 }}>
        用量与分摊
      </Typography.Title>
      <Typography.Paragraph type="secondary">
        口径：Kubernetes <code>resourceLimits</code>（非实际利用率）。表头旁{' '}
        <QuestionCircleOutlined /> 可看各列公式。最右 <strong>综合分摊占比</strong> 为时长、CPU、内存三项占比的等权平均，各租户相加为
        100%。
      </Typography.Paragraph>

      {data?.shareFormula?.composite ? (
        <Alert
          type="info"
          showIcon
          style={{ marginBottom: 16 }}
          message="综合分摊占比公式"
          description={data.shareFormula.composite}
        />
      ) : null}
      {data?.coverageNote ? (
        <Alert type="info" showIcon message={data.coverageNote} style={{ marginBottom: 16 }} />
      ) : null}
      {error ? <Alert type="error" showIcon message={error} style={{ marginBottom: 16 }} /> : null}

      <Card size="small" style={{ marginBottom: 16 }}>
        <Space wrap>
          {isAdmin && (
            <AdminTenantSelect value={tenantFilter} onChange={setTenantFilter} width={260} />
          )}
          <UsagePeriodFilterBar
            range={range}
            onRangeChange={(r) => {
              if (r) setRange(r);
            }}
            trailing={
              <Button type="primary" loading={loading} onClick={() => void load(range)}>
                查询
              </Button>
            }
          />
        </Space>
        {data?.period ? (
          <Typography.Text type="secondary" style={{ display: 'block', marginTop: 8 }}>
            区间（UTC）：{data.period.from} — {data.period.to}
          </Typography.Text>
        ) : null}
      </Card>

      {data?.totals ? (
        <Row gutter={16} style={{ marginBottom: 16 }}>
          <Col xs={24} sm={12} md={6}>
            <Card>
              <Statistic
                title="沙箱数"
                value={data.totals.sandboxCount ?? 0}
                formatter={(val) =>
                  usagePeriod?.from && usagePeriod?.to && Number(val) > 0 ? (
                    <Link
                      to={historySandboxesPath({
                        from: usagePeriod.from,
                        to: usagePeriod.to,
                        tenant: isAdmin ? tenantFilterToQuery(tenantFilter) : undefined,
                      })}
                    >
                      {val}
                    </Link>
                  ) : (
                    val
                  )
                }
              />
            </Card>
          </Col>
          <Col xs={24} sm={12} md={6}>
            <Card>
              <Statistic
                title="总占用时长"
                value={formatDuration(data.totals.overlapSeconds)}
              />
            </Card>
          </Col>
          <Col xs={24} sm={12} md={6}>
            <Card>
              <Statistic title="总 CPU·秒" value={data.totals.cpuCoreSeconds ?? 0} precision={1} />
            </Card>
          </Col>
          <Col xs={24} sm={12} md={6}>
            <Card>
              <Statistic
                title="总内存·Gi·秒"
                value={data.totals.memoryGiSeconds ?? 0}
                precision={1}
              />
            </Card>
          </Col>
        </Row>
      ) : null}

      <Table
        rowKey="tenant"
        loading={loading}
        columns={columns}
        dataSource={data?.tenants ?? []}
        pagination={false}
        size="middle"
      />
    </div>
  );
}
