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

import { Alert, Button, Card, Col, Descriptions, InputNumber, Modal, Row, Space, Tabs, Tag, Typography, message } from 'antd';
import { useCallback, useEffect, useMemo, useState } from 'react';
import { useNavigate, useParams, useSearchParams } from 'react-router-dom';

import { adminApi, sandboxApi } from '../api/client';
import { SandboxArchiveLogPanel } from '../components/SandboxArchiveLogPanel';
import { SandboxLogPanel } from '../components/SandboxLogPanel';
import type { Sandbox } from '../api/types';
import { useAuth } from '../auth/AuthContext';
import { formatDuration, sandboxDisplayName } from '../utils/format';

export function SandboxDetailPage() {
  const { id } = useParams<{ id: string }>();
  const [searchParams] = useSearchParams();
  const navigate = useNavigate();
  const { user } = useAuth();
  const adminTenant = searchParams.get('tenant') ?? '';
  const isAdminProxy = user?.role === 'admin' && Boolean(adminTenant);
  const defaultTab = searchParams.get('tab') === 'logs' ? 'logs' : 'overview';
  const [sandbox, setSandbox] = useState<Sandbox | null>(null);
  const [loading, setLoading] = useState(true);
  const [renewOpen, setRenewOpen] = useState(false);
  const [renewHours, setRenewHours] = useState(1);
  const [endpointPort, setEndpointPort] = useState<number>(8080);
  const [endpointInfo, setEndpointInfo] = useState<Record<string, unknown> | null>(null);
  const [actionLoading, setActionLoading] = useState(false);

  const api = useMemo(
    () =>
      isAdminProxy && id
        ? {
            get: () => adminApi.getSandbox(id, adminTenant),
            remove: () => adminApi.removeSandbox(id, adminTenant),
            renew: (expiresAt: string) => adminApi.renewSandbox(id, adminTenant, expiresAt),
            pause: () => adminApi.pauseSandbox(id, adminTenant),
            resume: () => adminApi.resumeSandbox(id, adminTenant),
            endpoint: (port: number) => adminApi.sandboxEndpoint(id, adminTenant, port),
          }
        : id
          ? {
              get: () => sandboxApi.get(id),
              remove: () => sandboxApi.remove(id),
              renew: (expiresAt: string) => sandboxApi.renewExpiration(id, expiresAt),
              pause: () => sandboxApi.pause(id),
              resume: () => sandboxApi.resume(id),
              endpoint: (port: number) => sandboxApi.endpoint(id, port),
            }
          : null,
    [adminTenant, id, isAdminProxy],
  );

  const load = useCallback(async () => {
    if (!api) return;
    setLoading(true);
    try {
      setSandbox(await api.get());
    } catch (e) {
      message.error(e instanceof Error ? e.message : '加载失败');
    } finally {
      setLoading(false);
    }
  }, [api]);

  useEffect(() => {
    if (user?.role === 'admin' && !adminTenant) {
      message.warning('Admin 查看详情需 URL 参数 tenant=');
      setLoading(false);
      return;
    }
    void load();
  }, [load, user?.role, adminTenant]);

  const state = sandbox?.status?.state;

  const onDelete = () => {
    if (!api) return;
    Modal.confirm({
      title: '确认删除沙箱？',
      content: '此操作不可撤销。',
      okType: 'danger',
      onOk: async () => {
        await api.remove();
        message.success('已删除');
        navigate(isAdminProxy ? '/admin/sandboxes' : '/sandboxes');
      },
    });
  };

  const onRenew = async () => {
    if (!api) return;
    const expiresAt = new Date(Date.now() + renewHours * 3_600_000).toISOString();
    try {
      await api.renew(expiresAt);
      message.success('已续期');
      setRenewOpen(false);
      await load();
    } catch (e) {
      message.error(e instanceof Error ? e.message : '续期失败');
    }
  };

  const onPauseResume = async (action: 'pause' | 'resume') => {
    if (!api) return;
    setActionLoading(true);
    try {
      if (action === 'pause') await api.pause();
      else await api.resume();
      message.success(action === 'pause' ? '已请求暂停' : '已请求恢复');
      await load();
    } catch (e) {
      message.error(e instanceof Error ? e.message : '操作失败');
    } finally {
      setActionLoading(false);
    }
  };

  const fetchEndpoint = async () => {
    if (!api) return;
    try {
      setEndpointInfo(await api.endpoint(endpointPort));
    } catch (e) {
      message.error(e instanceof Error ? e.message : '获取 endpoint 失败');
      setEndpointInfo(null);
    }
  };

  if (!id) return null;

  if (user?.role === 'admin' && !adminTenant) {
    return (
      <Alert
        type="warning"
        showIcon
        message="缺少 tenant 参数"
        description="请从「全局沙箱」列表进入，或访问 /sandboxes/{id}?tenant=租户名"
      />
    );
  }

  const logTenant = isAdminProxy ? adminTenant : undefined;

  return (
    <div>
      <Space style={{ marginBottom: 16 }}>
        <Typography.Title level={4} style={{ margin: 0 }}>
          {sandbox ? sandboxDisplayName(sandbox) : id}
        </Typography.Title>
        {state && <Tag color="blue">{state}</Tag>}
        {isAdminProxy && <Tag>{adminTenant}</Tag>}
      </Space>

      <Tabs
        defaultActiveKey={defaultTab}
        items={[
          {
            key: 'overview',
            label: '概览',
            children: (
              <Row gutter={[16, 16]}>
                <Col span={24}>
                  <Card loading={loading} title="状态">
                    <Descriptions column={1} bordered size="small">
                      <Descriptions.Item label="ID">{sandbox?.id}</Descriptions.Item>
                      <Descriptions.Item label="状态">{state ?? '—'}</Descriptions.Item>
                      <Descriptions.Item label="消息">{sandbox?.status?.message ?? '—'}</Descriptions.Item>
                      <Descriptions.Item label="运行时长">
                        {formatDuration(sandbox?.runtimeSummary?.wallClockSeconds)}
                      </Descriptions.Item>
                      <Descriptions.Item label="剩余">
                        {sandbox?.runtimeSummary?.remainingSeconds !== undefined
                          ? formatDuration(sandbox.runtimeSummary.remainingSeconds)
                          : '—'}
                      </Descriptions.Item>
                      <Descriptions.Item label="expiresAt">{sandbox?.expiresAt ?? '—'}</Descriptions.Item>
                      <Descriptions.Item label="镜像">{sandbox?.image?.uri ?? '—'}</Descriptions.Item>
                      <Descriptions.Item label="entrypoint">
                        {sandbox?.entrypoint?.join(' ') ?? '—'}
                      </Descriptions.Item>
                    </Descriptions>
                    <Space style={{ marginTop: 16 }} wrap>
                      <Button onClick={() => setRenewOpen(true)}>续期</Button>
                      <Button danger onClick={onDelete}>
                        删除
                      </Button>
                      <Button
                        disabled={state !== 'Running'}
                        loading={actionLoading}
                        onClick={() => void onPauseResume('pause')}
                      >
                        暂停
                      </Button>
                      <Button
                        disabled={state !== 'Paused'}
                        loading={actionLoading}
                        onClick={() => void onPauseResume('resume')}
                      >
                        恢复
                      </Button>
                      <Button onClick={() => void load()}>刷新</Button>
                    </Space>
                  </Card>
                </Col>
                <Col span={24}>
                  <Card title="Endpoint">
                    <Space wrap>
                      <InputNumber
                        min={1}
                        max={65535}
                        value={endpointPort}
                        onChange={(v) => setEndpointPort(v ?? 8080)}
                      />
                      <Button onClick={() => void fetchEndpoint()}>获取端口信息</Button>
                    </Space>
                    {endpointInfo && (
                      <pre style={{ marginTop: 16, background: '#f5f5f5', padding: 12, overflow: 'auto' }}>
                        {JSON.stringify(endpointInfo, null, 2)}
                      </pre>
                    )}
                  </Card>
                </Col>
              </Row>
            ),
          },
          {
            key: 'logs',
            label: '运行日志',
            children: (
              <Card title="日志">
                <Tabs
                  items={[
                    {
                      key: 'live',
                      label: '实时（Lifecycle）',
                      children: (
                        <SandboxLogPanel sandboxId={id} tenant={logTenant} autoLoad />
                      ),
                    },
                    {
                      key: 'archive',
                      label: '归档（Node Agent）',
                      children: (
                        <SandboxArchiveLogPanel sandboxId={id} tenant={logTenant} autoLoad />
                      ),
                    },
                  ]}
                />
              </Card>
            ),
          },
        ]}
      />

      <Modal
        title="续期"
        open={renewOpen}
        onOk={() => void onRenew()}
        onCancel={() => setRenewOpen(false)}
      >
        <Typography.Paragraph>新的过期时间为当前时间 + 指定小时数（UTC）。</Typography.Paragraph>
        <InputNumber min={1} value={renewHours} onChange={(v) => setRenewHours(v ?? 1)} addonAfter="小时" />
      </Modal>
    </div>
  );
}
