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

import { Card, Col, Row, Statistic, Typography, Alert } from 'antd';
import { useEffect, useState } from 'react';

import { adminApi, sandboxApi } from '../api/client';
import type { RuntimeStats } from '../api/types';
import { useAuth } from '../auth/AuthContext';
import { formatDuration } from '../utils/format';

export function OverviewPage() {
  const { user } = useAuth();
  const [stats, setStats] = useState<RuntimeStats | null>(null);
  const [tenantSummary, setTenantSummary] = useState<{
    total: number;
    running: number;
    expiring: number;
  } | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    void (async () => {
      setError(null);
      try {
        if (user?.role === 'admin') {
          const data = await adminApi.runtimeStats();
          setStats(data);
          setTenantSummary(null);
        } else {
          const list = await sandboxApi.list({ pageSize: 500 });
          const items = list.items ?? [];
          let running = 0;
          let expiring = 0;
          for (const s of items) {
            if (s.status?.state === 'Running') running += 1;
            const rem = s.runtimeSummary?.remainingSeconds;
            if (typeof rem === 'number' && rem <= 30 * 60) expiring += 1;
          }
          setTenantSummary({ total: items.length, running, expiring });
          setStats(null);
        }
      } catch (e) {
        setError(e instanceof Error ? e.message : '加载概览失败');
      }
    })();
  }, [user?.role]);

  return (
    <div>
      <Typography.Title level={4}>概览</Typography.Title>
      {error && <Alert type="error" message={error} style={{ marginBottom: 16 }} />}

      {user?.role === 'admin' && stats && (
        <Row gutter={[16, 16]}>
          <Col xs={24} sm={12} md={8}>
            <Card>
              <Statistic title="Running" value={stats.runningCount} />
            </Card>
          </Col>
          <Col xs={24} sm={12} md={8}>
            <Card>
              <Statistic
                title="平均运行时长"
                value={formatDuration(stats.avgWallClockSeconds)}
              />
            </Card>
          </Col>
          <Col xs={24} sm={12} md={8}>
            <Card>
              <Statistic
                title="30 分钟内到期"
                value={stats.expiringWithin30mCount}
              />
            </Card>
          </Col>
          <Col xs={24} sm={12} md={8}>
            <Card>
              <Statistic
                title="最大运行时长"
                value={formatDuration(stats.maxWallClockSeconds)}
              />
            </Card>
          </Col>
          <Col xs={24} sm={12} md={8}>
            <Card>
              <Statistic
                title="累计 wall-clock"
                value={formatDuration(stats.totalWallClockSeconds)}
              />
            </Card>
          </Col>
          <Col xs={24} sm={12} md={8}>
            <Card>
              <Statistic title="统计时间" value={stats.asOf} valueStyle={{ fontSize: 14 }} />
            </Card>
          </Col>
        </Row>
      )}

      {user?.role === 'tenant' && tenantSummary && (
        <Row gutter={[16, 16]}>
          <Col xs={24} sm={8}>
            <Card>
              <Statistic title="沙箱总数（当前页聚合）" value={tenantSummary.total} />
            </Card>
          </Col>
          <Col xs={24} sm={8}>
            <Card>
              <Statistic title="Running" value={tenantSummary.running} />
            </Card>
          </Col>
          <Col xs={24} sm={8}>
            <Card>
              <Statistic title="30 分钟内到期" value={tenantSummary.expiring} />
            </Card>
          </Col>
        </Row>
      )}
    </div>
  );
}
