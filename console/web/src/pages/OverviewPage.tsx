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

import { Alert, Card, Col, Row, Statistic, Typography } from 'antd';
import { useEffect, useState } from 'react';

import { sandboxApi } from '../api/client';
import { useAuth } from '../auth/AuthContext';

export function OverviewPage() {
  const { user } = useAuth();
  const [total, setTotal] = useState(0);
  const [running, setRunning] = useState(0);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (user?.role === 'admin') return;
    void (async () => {
      setError(null);
      try {
        const [all, runningList] = await Promise.all([
          sandboxApi.list({ page: 1, pageSize: 1 }),
          sandboxApi.list({ page: 1, pageSize: 1, state: 'Running' }),
        ]);
        setTotal(all.pagination?.totalItems ?? all.items?.length ?? 0);
        setRunning(runningList.pagination?.totalItems ?? runningList.items?.length ?? 0);
      } catch (e) {
        setError(e instanceof Error ? e.message : 'Failed to load overview');
      }
    })();
  }, [user?.role]);

  if (user?.role === 'admin') {
    return (
      <div>
        <Typography.Title level={4}>概览</Typography.Title>
        <Alert type="info" message="Admin 聚合视图将在 PR3 提供。" />
      </div>
    );
  }

  return (
    <div>
      <Typography.Title level={4}>概览</Typography.Title>
      {error && <Alert type="error" message={error} style={{ marginBottom: 16 }} />}
      <Row gutter={[16, 16]}>
        <Col xs={24} sm={12}>
          <Card>
            <Statistic title="沙箱总数" value={total} />
          </Card>
        </Col>
        <Col xs={24} sm={12}>
          <Card>
            <Statistic title="Running" value={running} />
          </Card>
        </Col>
      </Row>
    </div>
  );
}
