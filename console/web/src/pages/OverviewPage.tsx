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

import {
  Alert,
  Button,
  Card,
  Empty,
  Progress,
  Space,
  Spin,
  Statistic,
  Table,
  Typography,
} from 'antd';
import {
  ClockCircleOutlined,
  CloudServerOutlined,
  DatabaseOutlined,
  PlayCircleOutlined,
  ThunderboltOutlined,
} from '@ant-design/icons';
import type { ColumnsType } from 'antd/es/table';
import { useEffect, useMemo, useState } from 'react';
import { Link } from 'react-router-dom';

import { adminApi, ApiError, historyApi, sandboxApi } from '../api/client';
import { AdminTenantSelect, ALL_TENANTS, tenantFilterToQuery } from '../components/AdminTenantSelect';
import { USAGE_METRIC_HINTS, UsageMetricTitle } from '../components/UsageMetricTitle';
import type { SandboxHistoryStats, TenantUsageRow } from '../api/types';
import { useAuth } from '../auth/AuthContext';
import { SandboxStateTag, TenantTag } from '../components/SemanticTags';
import { formatDuration } from '../utils/format';
import { historySandboxesPath } from '../utils/historyLinks';
import {
  currentMonthRangeUtc,
  monthRangeToUsageQuery,
  previousMonthRangeUtc,
} from '../utils/usagePeriod';

import './OverviewPage.css';

function shareProgressColor(percent: number): string {
  if (percent >= 40) return '#1677ff';
  if (percent >= 20) return '#13c2c2';
  if (percent >= 10) return '#52c41a';
  return '#faad14';
}

type LiveCounts = {
  total: number;
  running: number;
};

type UsageTotals = {
  sandboxCount?: number;
  overlapSeconds?: number;
  cpuCoreSeconds?: number;
  memoryGiSeconds?: number;
};

function pctChange(current: number, previous: number | undefined): number | null {
  if (previous === undefined) return null;
  if (previous === 0) {
    if (current === 0) return 0;
    return null;
  }
  return ((current - previous) / previous) * 100;
}

function DeltaTag({ current, previous }: { current: number; previous?: number }) {
  const pct = pctChange(current, previous);
  if (pct === null) {
    return previous === 0 && current > 0 ? (
      <span className="overview-kpi-delta up">↑ 新</span>
    ) : null;
  }
  if (Math.abs(pct) < 0.05) {
    return <span className="overview-kpi-delta flat">— 0%</span>;
  }
  const up = pct > 0;
  return (
    <span className={`overview-kpi-delta ${up ? 'up' : 'down'}`}>
      {up ? '↑' : '↓'} {Math.abs(pct).toFixed(0)}%
    </span>
  );
}

function MiniSparkline({ seed }: { seed: number }) {
  const points = useMemo(() => {
    const w = 88;
    const h = 32;
    const coords: string[] = [];
    let v = (seed % 97) + 3;
    for (let i = 0; i < 8; i++) {
      v = (v * 17 + seed + i * 11) % 24;
      const y = h - 6 - v;
      coords.push(`${(i / 7) * w},${y}`);
    }
    return coords.join(' ');
  }, [seed]);

  return (
    <svg className="overview-kpi-spark" width={88} height={32} viewBox="0 0 88 32" aria-hidden>
      <polyline fill="none" stroke="#1677ff" strokeWidth="2" strokeLinecap="round" points={points} />
    </svg>
  );
}

function UsageKpiCard(props: {
  icon: React.ReactNode;
  iconTone: 'blue' | 'purple' | 'cyan' | 'orange';
  title: string;
  displayValue: string;
  numericCurrent: number;
  numericPrevious?: number;
  sparkSeed: number;
  detailTo?: string;
}) {
  const { icon, iconTone, title, displayValue, numericCurrent, numericPrevious, sparkSeed, detailTo } =
    props;
  const valueNode =
    detailTo && numericCurrent > 0 ? (
      <Link to={detailTo} className="overview-kpi-value-link">
        {displayValue}
      </Link>
    ) : (
      displayValue
    );
  return (
    <Card className="overview-kpi-card" bordered={false}>
      <div className="overview-kpi-top">
        <div className={`overview-kpi-icon ${iconTone}`}>{icon}</div>
        <DeltaTag current={numericCurrent} previous={numericPrevious} />
      </div>
      <div className="overview-kpi-title">{title}</div>
      <div className="overview-kpi-value">{valueNode}</div>
      <MiniSparkline seed={sparkSeed} />
    </Card>
  );
}

function LiveRuntimePanel({
  total,
  running,
  isAdmin,
}: {
  total: number;
  running: number;
  isAdmin: boolean;
}) {
  return (
    <Card className="overview-live-card" bordered={false}>
      <div className="overview-live-stats">
        <div className="overview-live-stat overview-live-stat--total">
          <div className="overview-live-stat-head">
            <div className="overview-live-stat-icon overview-live-stat-icon--blue" aria-hidden>
              <CloudServerOutlined />
            </div>
            <Typography.Text type="secondary" className="overview-live-stat-label">
              列表中的沙箱数
            </Typography.Text>
          </div>
          <div className="overview-live-stat-value overview-live-stat-value--blue">{total}</div>
          <Typography.Text type="secondary" className="overview-live-stat-foot">
            当前 API 分页列表（最多 200 条）
          </Typography.Text>
        </div>
        <div className="overview-live-stat-divider" aria-hidden />
        <div
          className={`overview-live-stat overview-live-stat--running${
            running > 0 ? ' overview-live-stat--running-active' : ''
          }`}
        >
          <div className="overview-live-stat-head">
            <div
              className={`overview-live-stat-icon${
                running > 0 ? ' overview-live-stat-icon--green' : ' overview-live-stat-icon--muted'
              }`}
              aria-hidden
            >
              <PlayCircleOutlined />
            </div>
            <span className="overview-live-stat-label-row">
              <SandboxStateTag state="Running" />
            </span>
          </div>
          <div
            className={`overview-live-stat-value${
              running > 0 ? ' overview-live-stat-value--green' : ' overview-live-stat-value--muted'
            }`}
          >
            {running}
          </div>
          <Typography.Text type="secondary" className="overview-live-stat-foot">
            状态为 Running 的沙箱
          </Typography.Text>
        </div>
      </div>
      <div className="overview-live-hint">
        <Typography.Text type="secondary">
          数据来源：Lifecycle 实时接口，与下方 PostgreSQL 历史统计独立。
          {isAdmin ? ' Admin 可按页面顶部租户筛选缩小范围。' : ''}
          {total > 0 && running === 0 ? ' 当前无运行中实例。' : null}
        </Typography.Text>
        <Link to={isAdmin ? '/admin/sandboxes' : '/sandboxes'} className="overview-live-hint-link">
          打开实时列表 →
        </Link>
      </div>
    </Card>
  );
}

export function OverviewPage() {
  const { user } = useAuth();
  const isAdmin = user?.role === 'admin';
  const [tenantFilter, setTenantFilter] = useState(ALL_TENANTS);
  const [loading, setLoading] = useState(true);
  const [historyOff, setHistoryOff] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [stats, setStats] = useState<SandboxHistoryStats | null>(null);
  const [monthTenants, setMonthTenants] = useState<TenantUsageRow[]>([]);
  const [monthTotals, setMonthTotals] = useState<UsageTotals | null>(null);
  const [prevMonthTotals, setPrevMonthTotals] = useState<UsageTotals | null>(null);
  const [live, setLive] = useState<LiveCounts | null>(null);

  const tenantQuery = isAdmin ? tenantFilterToQuery(tenantFilter) : undefined;

  useEffect(() => {
    void (async () => {
      setLoading(true);
      setError(null);
      setHistoryOff(false);
      setStats(null);
      setMonthTenants([]);
      setMonthTotals(null);
      setPrevMonthTotals(null);
      setLive(null);

      const monthRange = currentMonthRangeUtc();
      const usageQuery = monthRangeToUsageQuery(monthRange);
      const prevUsageQuery = monthRangeToUsageQuery(previousMonthRangeUtc());

      try {
        const livePromise = isAdmin
          ? adminApi.sandboxes({ pageSize: 200, tenant: tenantQuery }).then((data) => {
              const items = data.items ?? [];
              return {
                total: items.length,
                running: items.filter((s) => s.status?.state === 'Running').length,
              };
            })
          : sandboxApi.list({ pageSize: 200 }).then((data) => {
              const items = data.items ?? [];
              return {
                total: items.length,
                running: items.filter((s) => s.status?.state === 'Running').length,
              };
            });

        const historyBlock = (async () => {
          try {
            const tenantArg = tenantQuery ? { tenant: tenantQuery } : {};
            const [st, usage, prevUsage] = await Promise.all([
              historyApi.stats(tenantQuery),
              historyApi.usage({ ...usageQuery, ...tenantArg }),
              historyApi.usage({ ...prevUsageQuery, ...tenantArg }),
            ]);
            if (st.enabled === false) {
              setHistoryOff(true);
              return;
            }
            setStats(st);
            setMonthTotals(usage.totals ?? null);
            setPrevMonthTotals(prevUsage.totals ?? null);
            setMonthTenants(usage.tenants ?? []);
          } catch (e) {
            if (e instanceof ApiError && e.status === 503) {
              setHistoryOff(true);
              return;
            }
            throw e;
          }
        })();

        const [liveCounts] = await Promise.all([livePromise, historyBlock]);
        setLive(liveCounts);
      } catch (e) {
        setError(e instanceof Error ? e.message : '加载概览失败');
      } finally {
        setLoading(false);
      }
    })();
  }, [isAdmin, tenantFilter, tenantQuery]);

  const monthLabel = useMemo(() => {
    const [start] = currentMonthRangeUtc();
    return start.format('YYYY-MM');
  }, []);

  const monthUsagePeriod = useMemo(() => monthRangeToUsageQuery(currentMonthRangeUtc()), [monthLabel]);

  const historyEmpty =
    !historyOff &&
    stats &&
    (stats.totalRecords ?? 0) === 0 &&
    (monthTotals?.sandboxCount ?? 0) === 0;

  const tenantColumns: ColumnsType<TenantUsageRow> = useMemo(
    () => [
      {
        title: '租户',
        dataIndex: 'tenant',
        width: 140,
        render: (t: string) => (
          <TenantTag tenant={t} className="overview-tenant-tag" />
        ),
      },
      {
        title: (
          <UsageMetricTitle label="本月沙箱数" hint={USAGE_METRIC_HINTS.sandboxCount} />
        ),
        dataIndex: 'sandboxCount',
        width: 120,
        render: (v: number, row) =>
          v > 0 ? (
            <Link
              to={historySandboxesPath({
                from: monthUsagePeriod.from,
                to: monthUsagePeriod.to,
                tenant: row.tenant,
              })}
            >
              <Typography.Text strong className="overview-tenant-metric overview-tenant-metric--count">
                {v}
              </Typography.Text>
            </Link>
          ) : (
            <Typography.Text strong className="overview-tenant-metric overview-tenant-metric--count">
              {v}
            </Typography.Text>
          ),
      },
      {
        title: <UsageMetricTitle label="占用时长" hint={USAGE_METRIC_HINTS.overlapSeconds} />,
        dataIndex: 'overlapSeconds',
        render: (v: number) => (
          <span className="overview-tenant-metric overview-tenant-metric--time">{formatDuration(v)}</span>
        ),
      },
      {
        title: <UsageMetricTitle label="CPU·秒" hint={USAGE_METRIC_HINTS.cpuCoreSeconds} />,
        dataIndex: 'cpuCoreSeconds',
        render: (v: number) => (
          <span className="overview-tenant-metric overview-tenant-metric--cpu">
            {v.toLocaleString(undefined, { maximumFractionDigits: 1 })}
          </span>
        ),
      },
      {
        title: <UsageMetricTitle label="内存·Gi·秒" hint={USAGE_METRIC_HINTS.memoryGiSeconds} />,
        dataIndex: 'memoryGiSeconds',
        render: (v: number) => (
          <span className="overview-tenant-metric overview-tenant-metric--mem">
            {v.toLocaleString(undefined, { maximumFractionDigits: 1 })}
          </span>
        ),
      },
      {
        title: '时长占比',
        key: 'share',
        width: 140,
        render: (_, r) => {
          const pct = r.sharePercent?.time ?? 0;
          return (
            <div className="overview-tenant-share">
              <Progress
                percent={pct}
                size="small"
                showInfo={false}
                strokeColor={shareProgressColor(pct)}
                trailColor="rgba(0,0,0,0.06)"
              />
              <Typography.Text strong className="overview-tenant-share-pct">
                {pct}%
              </Typography.Text>
            </div>
          );
        },
      },
    ],
    [monthUsagePeriod.from, monthUsagePeriod.to],
  );

  const quickLinks: { to: string; label: string }[] = isAdmin
    ? [
        { to: '/admin/sandboxes', label: '全局沙箱（实时）' },
        { to: '/history/sandboxes', label: '申请历史' },
        { to: '/history/usage', label: '用量分摊' },
        { to: '/platform/health', label: '组件健康' },
      ]
    : [
        { to: '/sandboxes', label: '我的沙箱（实时）' },
        { to: '/history/sandboxes', label: '申请历史' },
        { to: '/history/usage', label: '用量分摊' },
        { to: '/sandboxes/new', label: '创建沙箱' },
      ];

  const sandboxCount = monthTotals?.sandboxCount ?? 0;
  const overlapSec = monthTotals?.overlapSeconds ?? 0;
  const cpuSec = monthTotals?.cpuCoreSeconds ?? 0;
  const memGiSec = monthTotals?.memoryGiSeconds ?? 0;

  return (
    <div className="overview-page">
      <Space style={{ width: '100%', justifyContent: 'space-between', marginBottom: 16 }} align="start">
        <div>
          <Typography.Title level={3} style={{ marginTop: 0, marginBottom: 6, fontWeight: 600 }}>
            概览
          </Typography.Title>
          <Typography.Text type="secondary" style={{ fontSize: 14 }}>
            {isAdmin ? '平台视角' : `租户 ${user?.tenant ?? ''}`} · 本月 {monthLabel}（UTC）
          </Typography.Text>
        </div>
        {isAdmin && <AdminTenantSelect value={tenantFilter} onChange={setTenantFilter} width={220} />}
      </Space>

      <nav className="overview-quick-nav" aria-label="快捷入口">
        {quickLinks.map((item) => (
          <Link key={item.to} to={item.to}>
            <Button block className="overview-quick-nav-btn">
              {item.label}
            </Button>
          </Link>
        ))}
      </nav>

      {historyOff && (
        <Alert
          type="warning"
          showIcon
          style={{ marginBottom: 16 }}
          message="历史统计未启用"
          description="配置 BFF_HISTORY_ENABLED 与 BFF_HISTORY_DATABASE_URL 并重启 Console BFF 后，方可看到本月用量与历史累计。"
        />
      )}
      {error && <Alert type="error" message={error} style={{ marginBottom: 16 }} />}

      <Spin spinning={loading}>
        <Typography.Title level={5} className="overview-section-title">
          当前运行（Lifecycle 实时）
        </Typography.Title>
        <LiveRuntimePanel
          total={live?.total ?? 0}
          running={live?.running ?? 0}
          isAdmin={isAdmin}
        />

        {!historyOff && (
          <>
            <div className="overview-section-head">
              <Typography.Title level={5} className="overview-section-title" style={{ marginBottom: 0 }}>
                本月用量（limits 口径）
              </Typography.Title>
              <Link to="/history/usage">详细分摊 →</Link>
            </div>
            <div className="overview-kpi-grid">
              <UsageKpiCard
                icon={<CloudServerOutlined />}
                iconTone="blue"
                title="沙箱数"
                displayValue={String(sandboxCount)}
                numericCurrent={sandboxCount}
                numericPrevious={prevMonthTotals?.sandboxCount}
                sparkSeed={sandboxCount + 1}
                detailTo={historySandboxesPath({
                  from: monthUsagePeriod.from,
                  to: monthUsagePeriod.to,
                  tenant: tenantQuery,
                })}
              />
              <UsageKpiCard
                icon={<ClockCircleOutlined />}
                iconTone="purple"
                title="占用时长"
                displayValue={formatDuration(overlapSec)}
                numericCurrent={overlapSec}
                numericPrevious={prevMonthTotals?.overlapSeconds}
                sparkSeed={overlapSec + 2}
              />
              <UsageKpiCard
                icon={<ThunderboltOutlined />}
                iconTone="cyan"
                title="CPU·秒"
                displayValue={cpuSec.toLocaleString(undefined, { maximumFractionDigits: 1 })}
                numericCurrent={cpuSec}
                numericPrevious={prevMonthTotals?.cpuCoreSeconds}
                sparkSeed={Math.floor(cpuSec) + 3}
              />
              <UsageKpiCard
                icon={<DatabaseOutlined />}
                iconTone="orange"
                title="内存·Gi·秒"
                displayValue={memGiSec.toLocaleString(undefined, { maximumFractionDigits: 1 })}
                numericCurrent={memGiSec}
                numericPrevious={prevMonthTotals?.memoryGiSeconds}
                sparkSeed={Math.floor(memGiSec) + 4}
              />
            </div>

            {isAdmin && !tenantQuery && monthTenants.length > 0 && (
              <>
                <Typography.Title level={5} className="overview-section-title">
                  租户用量统计
                </Typography.Title>
                <Card className="overview-tenant-card" bordered={false}>
                  <Table
                    className="overview-tenant-table"
                    size="middle"
                    rowKey="tenant"
                    pagination={false}
                    columns={tenantColumns}
                    dataSource={monthTenants}
                    rowClassName={(_, index) =>
                      index % 2 === 0 ? 'overview-tenant-row-even' : 'overview-tenant-row-odd'
                    }
                  />
                </Card>
              </>
            )}

            <Typography.Title level={5} className="overview-section-title">
              历史累计（Console 入库以来）
            </Typography.Title>
            <div className="overview-history-grid">
              <Card className="overview-history-card" bordered={false}>
                <Statistic title="历史记录总数" value={stats?.totalRecords ?? 0} />
              </Card>
              <Card className="overview-history-card" bordered={false}>
                <Statistic title="进行中（未结束）" value={stats?.activeRecords ?? 0} />
              </Card>
              <Card className="overview-history-card" bordered={false}>
                <Statistic
                  title="累计运行时长"
                  value={formatDuration(stats?.totalWallClockSeconds ?? 0)}
                />
              </Card>
              <Card className="overview-history-card" bordered={false}>
                <Statistic
                  title="平均运行时长"
                  value={
                    (stats?.totalRecords ?? 0) > 0 ? formatDuration(stats?.avgWallClockSeconds) : '—'
                  }
                />
              </Card>
            </div>
          </>
        )}

        {historyEmpty && (
          <Empty
            style={{ marginTop: 32 }}
            description={
              <div style={{ maxWidth: 480, margin: '0 auto', textAlign: 'left' }}>
                <Typography.Paragraph strong>为什么历史是 0？</Typography.Paragraph>
                <Typography.Paragraph type="secondary" style={{ marginBottom: 8 }}>
                  数据写在 PostgreSQL 表 <code>sandbox_lifecycle_history</code>。Server 开启生命周期审计后，SDK 创建也会入库；否则主要依赖 Console 或对账。
                </Typography.Paragraph>
                <ul style={{ paddingLeft: 20, margin: 0, color: 'rgba(0,0,0,0.45)' }}>
                  <li>打开本页会按租户拉 Lifecycle 现网列表并入库；若仍为 0，可能当前无沙箱或 Lifecycle 对账失败</li>
                  <li>启用历史之前已销毁的沙箱无法回填</li>
                  <li>若上方 Running &gt; 0 但历史仍为 0，检查 BFF DSN、租户 API Key、BFF 日志 reconcile / upsert 错误</li>
                </ul>
                <Space style={{ marginTop: 16 }}>
                  {isAdmin ? (
                    <Link to="/admin/sandboxes">
                      <Button type="primary">去看实时沙箱</Button>
                    </Link>
                  ) : (
                    <>
                      <Link to="/sandboxes/new">
                        <Button type="primary">创建沙箱</Button>
                      </Link>
                      <Link to="/sandboxes">
                        <Button>打开沙箱列表</Button>
                      </Link>
                    </>
                  )}
                </Space>
              </div>
            }
          />
        )}
      </Spin>
    </div>
  );
}
