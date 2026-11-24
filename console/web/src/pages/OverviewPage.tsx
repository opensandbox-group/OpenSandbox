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
import { useI18n } from '../i18n';
import { formatDateTime, formatDuration } from '../utils/format';

export function OverviewPage() {
  const { user } = useAuth();
  const { t } = useI18n();
  const [stats, setStats] = useState<RuntimeStats | null>(null);
  const [tenantSummary, setTenantSummary] = useState<{
    total: number;
    running: number;
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
          const [all, runningList] = await Promise.all([
            sandboxApi.list({ page: 1, pageSize: 1 }),
            sandboxApi.list({ page: 1, pageSize: 1, state: 'Running' }),
          ]);
          setTenantSummary({
            total: all.pagination?.totalItems ?? all.items?.length ?? 0,
            running: runningList.pagination?.totalItems ?? runningList.items?.length ?? 0,
          });
          setStats(null);
        }
      } catch (e) {
        setError(e instanceof Error ? e.message : t('overview.loadFailed'));
      }
    })();
  }, [user?.role, t]);

  return (
    <div>
      <Typography.Title level={4}>{t('common.overview')}</Typography.Title>
      {error && <Alert type="error" message={error} style={{ marginBottom: 16 }} />}

      {user?.role === 'admin' && stats && (
        <Row gutter={[16, 16]}>
          <Col xs={24} sm={12} md={8}>
            <Card>
              <Statistic title={t('overview.running')} value={stats.runningCount} />
            </Card>
          </Col>
          <Col xs={24} sm={12} md={8}>
            <Card>
              <Statistic
                title={t('overview.averageUptime')}
                value={formatDuration(stats.avgWallClockSeconds)}
              />
            </Card>
          </Col>
          <Col xs={24} sm={12} md={8}>
            <Card>
              <Statistic
                title={t('overview.expiresIn30Minutes')}
                value={stats.expiringWithin30mCount}
              />
            </Card>
          </Col>
          <Col xs={24} sm={12} md={8}>
            <Card>
              <Statistic
                title={t('overview.maximumUptime')}
                value={formatDuration(stats.maxWallClockSeconds)}
              />
            </Card>
          </Col>
          <Col xs={24} sm={12} md={8}>
            <Card>
              <Statistic
                title={t('overview.totalWallClockTime')}
                value={formatDuration(stats.totalWallClockSeconds)}
              />
            </Card>
          </Col>
          <Col xs={24} sm={12} md={8}>
            <Card>
              <Statistic title={t('overview.statsUpdatedAt')} value={formatDateTime(stats.asOf)} />
            </Card>
          </Col>
        </Row>
      )}

      {user?.role === 'tenant' && tenantSummary && (
        <Row gutter={[16, 16]}>
          <Col xs={24} sm={8}>
            <Card>
              <Statistic title={t('overview.totalSandboxes')} value={tenantSummary.total} />
            </Card>
          </Col>
          <Col xs={24} sm={8}>
            <Card>
              <Statistic title={t('overview.running')} value={tenantSummary.running} />
            </Card>
          </Col>
        </Row>
      )}
    </div>
  );
}
