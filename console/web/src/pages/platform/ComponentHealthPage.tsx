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

import { Alert, Card, Col, Descriptions, Row, Spin, Tag, Typography } from 'antd';
import { useEffect, useState } from 'react';

import { adminApi, platformApi } from '../../api/client';
import type { PlatformSummary } from '../../api/types';
import { useAuth } from '../../auth/AuthContext';

export function ComponentHealthPage() {
  const { user } = useAuth();
  const [summary, setSummary] = useState<PlatformSummary | null>(null);
  const [bffHealth, setBffHealth] = useState<{ status?: string } | null>(null);
  const [loading, setLoading] = useState(true);
  const [usedFallback, setUsedFallback] = useState(false);

  useEffect(() => {
    void (async () => {
      setLoading(true);
      try {
        if (user?.role === 'admin') {
          setSummary(await adminApi.platformSummary());
          setUsedFallback(false);
        } else {
          setUsedFallback(true);
          setBffHealth(await platformApi.bffHealth());
        }
      } catch {
        setUsedFallback(true);
        try {
          setBffHealth(await platformApi.bffHealth());
        } catch {
          setBffHealth(null);
        }
      } finally {
        setLoading(false);
      }
    })();
  }, [user?.role]);

  if (loading) return <Spin />;

  return (
    <div>
      <Typography.Title level={4}>组件健康</Typography.Title>
      {usedFallback && (
        <Alert
          type="info"
          showIcon
          style={{ marginBottom: 16 }}
          message="未使用 Admin platform/summary，已回退到 BFF /health"
        />
      )}

      {summary && (
        <>
          <Card title="Lifecycle Server" style={{ marginBottom: 16 }}>
            <Descriptions column={1} bordered size="small">
              <Descriptions.Item label="Health">
                {summary.server?.health ? (
                  <Tag color="green">{summary.server.health.status ?? 'ok'}</Tag>
                ) : (
                  summary.server?.healthError ?? '—'
                )}
              </Descriptions.Item>
              <Descriptions.Item label="Version">
                <pre style={{ margin: 0 }}>{JSON.stringify(summary.server?.version, null, 2)}</pre>
              </Descriptions.Item>
            </Descriptions>
          </Card>
          <Row gutter={[16, 16]}>
            {Object.entries(summary.components ?? {}).map(([name, comp]) => (
              <Col xs={24} sm={12} md={8} key={name}>
                <Card title={name}>
                  <Tag
                    color={
                      comp.status === 'ok'
                        ? 'green'
                        : comp.status === 'degraded'
                          ? 'orange'
                          : comp.status === 'critical' || comp.status === 'error'
                            ? 'red'
                            : 'default'
                    }
                  >
                    {comp.status ?? 'unknown'}
                  </Tag>
                  <Typography.Paragraph type="secondary">{comp.note}</Typography.Paragraph>
                  {comp.readyPods != null && comp.totalPods != null && (
                    <Typography.Text type="secondary">
                      Ready pods: {comp.readyPods}/{comp.totalPods}
                    </Typography.Text>
                  )}
                  {comp.sampleReasons && comp.sampleReasons.length > 0 && (
                    <Typography.Paragraph type="secondary" style={{ marginBottom: 0 }}>
                      Reasons: {comp.sampleReasons.join(', ')}
                    </Typography.Paragraph>
                  )}
                </Card>
              </Col>
            ))}
          </Row>
        </>
      )}

      {!summary && bffHealth && (
        <Card>
          <Tag color="green">BFF {bffHealth.status}</Tag>
        </Card>
      )}
    </div>
  );
}
