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
import { statusLabel, useI18n } from '../i18n';
import { formatDuration, sandboxDisplayName } from '../utils/format';
import { ShellLink } from '../components/ShellLink';

export function SandboxDetailPage() {
  const { id } = useParams<{ id: string }>();
  const [searchParams] = useSearchParams();
  const navigate = useNavigate();
  const { user } = useAuth();
  const { t } = useI18n();
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
  const [renewLoading, setRenewLoading] = useState(false);
  const [deleteLoading, setDeleteLoading] = useState(false);
  const [endpointLoading, setEndpointLoading] = useState(false);

  const api = useMemo(() => {
    if (!id) return null;
    if (isAdminProxy) {
      return {
        get: () => adminApi.getSandbox(id, adminTenant),
        remove: () => adminApi.removeSandbox(id, adminTenant),
        renew: (expiresAt: string) => adminApi.renewSandbox(id, adminTenant, expiresAt),
        pause: () => adminApi.pauseSandbox(id, adminTenant),
        resume: () => adminApi.resumeSandbox(id, adminTenant),
        endpoint: (port: number) => adminApi.sandboxEndpoint(id, adminTenant, port),
      };
    }
    return {
      get: () => sandboxApi.get(id),
      remove: () => sandboxApi.remove(id),
      renew: (expiresAt: string) => sandboxApi.renewExpiration(id, expiresAt),
      pause: () => sandboxApi.pause(id),
      resume: () => sandboxApi.resume(id),
      endpoint: (port: number) => sandboxApi.endpoint(id, port),
    };
  }, [adminTenant, id, isAdminProxy]);

  const load = useCallback(async () => {
    if (!api) return;
    setLoading(true);
    try {
      setSandbox(await api.get());
    } catch (e) {
      message.error(e instanceof Error ? e.message : t('sandboxDetail.loadFailed'));
    } finally {
      setLoading(false);
    }
  }, [api, t]);

  useEffect(() => {
    if (user?.role === 'admin' && !adminTenant) {
      message.warning(t('sandboxDetail.adminTenantWarning'));
      setLoading(false);
      return;
    }
    void load();
  }, [load, user?.role, adminTenant, t]);

  const state = sandbox?.status?.state;

  const onDelete = () => {
    if (!api) return;
    Modal.confirm({
      title: t('sandboxDetail.deleteTitle'),
      content: t('sandboxDetail.deleteContent'),
      // antd static methods do not read ConfigProvider, so the action labels
      // are passed explicitly to keep the dialog in one language.
      okText: t('common.delete'),
      cancelText: t('common.cancel'),
      okType: 'danger',
      onOk: async () => {
        setDeleteLoading(true);
        try {
          await api.remove();
          message.success(t('sandboxDetail.deleted'));
          navigate(isAdminProxy ? '/admin/sandboxes' : '/sandboxes');
        } catch (e) {
          message.error(e instanceof Error ? e.message : t('sandboxDetail.deleteFailed'));
          throw e;
        } finally {
          setDeleteLoading(false);
        }
      },
    });
  };

  const onRenew = async () => {
    if (!api) return;
    const parsed = sandbox?.expiresAt ? Date.parse(sandbox.expiresAt) : Number.NaN;
    const base = Number.isNaN(parsed) ? Date.now() : parsed;
    const expiresAt = new Date(base + renewHours * 3_600_000).toISOString();
    setRenewLoading(true);
    try {
      await api.renew(expiresAt);
      message.success(t('sandboxDetail.renewed'));
      setRenewOpen(false);
      await load();
    } catch (e) {
      message.error(e instanceof Error ? e.message : t('sandboxDetail.renewFailed'));
    } finally {
      setRenewLoading(false);
    }
  };

  const onPauseResume = async (action: 'pause' | 'resume') => {
    if (!api) return;
    setActionLoading(true);
    try {
      if (action === 'pause') await api.pause();
      else await api.resume();
      message.success(
        action === 'pause' ? t('sandboxDetail.pauseRequested') : t('sandboxDetail.resumeRequested'),
      );
      await load();
    } catch (e) {
      message.error(e instanceof Error ? e.message : t('sandboxDetail.actionFailed'));
    } finally {
      setActionLoading(false);
    }
  };

  const fetchEndpoint = async () => {
    if (!api) return;
    setEndpointLoading(true);
    try {
      setEndpointInfo(await api.endpoint(endpointPort));
    } catch (e) {
      message.error(e instanceof Error ? e.message : t('sandboxDetail.endpointFailed'));
      setEndpointInfo(null);
    } finally {
      setEndpointLoading(false);
    }
  };

  if (!id) return null;

  if (user?.role === 'admin' && !adminTenant) {
    return (
      <Alert
        type="warning"
        showIcon
        message={t('sandboxDetail.missingTenant')}
        description={t('sandboxDetail.missingTenantDescription', {
          path: '/sandboxes/{id}?tenant=tenant-name',
        })}
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
        {state && <Tag color="blue">{statusLabel(t, state)}</Tag>}
        {isAdminProxy && <Tag>{adminTenant}</Tag>}
      </Space>

      <Tabs
        defaultActiveKey={defaultTab}
        items={[
          {
            key: 'overview',
            label: t('common.overview'),
            children: (
              <Row gutter={[16, 16]}>
                <Col span={24}>
                  <Card loading={loading} title={t('common.status')}>
                    <Descriptions column={1} bordered size="small">
                      <Descriptions.Item label="ID">{sandbox?.id}</Descriptions.Item>
                      <Descriptions.Item label={t('common.status')}>
                        {state ? statusLabel(t, state) : '—'}
                      </Descriptions.Item>
                      <Descriptions.Item label={t('sandboxDetail.message')}>
                        {sandbox?.status?.message ?? '—'}
                      </Descriptions.Item>
                      <Descriptions.Item label={t('common.uptime')}>
                        {formatDuration(sandbox?.runtimeSummary?.wallClockSeconds)}
                      </Descriptions.Item>
                      <Descriptions.Item label={t('common.remaining')}>
                        {sandbox?.runtimeSummary?.remainingSeconds !== undefined
                          ? formatDuration(sandbox.runtimeSummary.remainingSeconds)
                          : '—'}
                      </Descriptions.Item>
                      <Descriptions.Item label="expiresAt">{sandbox?.expiresAt ?? '—'}</Descriptions.Item>
                      <Descriptions.Item label={t('common.image')}>
                        {sandbox?.image?.uri ?? '—'}
                      </Descriptions.Item>
                      <Descriptions.Item label="entrypoint">
                        {sandbox?.entrypoint?.join(' ') ?? '—'}
                      </Descriptions.Item>
                    </Descriptions>
                    <Space style={{ marginTop: 16 }} wrap>
                      <Button onClick={() => setRenewOpen(true)}>{t('common.renew')}</Button>
                      <Button danger loading={deleteLoading} onClick={onDelete}>
                        {t('common.delete')}
                      </Button>
                      <Button
                        disabled={state !== 'Running'}
                        loading={actionLoading}
                        onClick={() => void onPauseResume('pause')}
                      >
                        {t('common.pause')}
                      </Button>
                      <Button
                        disabled={state !== 'Paused'}
                        loading={actionLoading}
                        onClick={() => void onPauseResume('resume')}
                      >
                        {t('common.resume')}
                      </Button>
                      <Button onClick={() => void load()}>{t('common.refresh')}</Button>
                      <ShellLink
                        sandboxId={id ?? ''}
                        state={state}
                        tenant={isAdminProxy ? adminTenant : undefined}
                        variant="button"
                      />
                    </Space>
                  </Card>
                </Col>
                <Col span={24}>
                  <Card title={t('sandboxDetail.endpoint')}>
                    <Space wrap>
                      <InputNumber
                        min={1}
                        max={65535}
                        value={endpointPort}
                        onChange={(v) => setEndpointPort(v ?? 8080)}
                      />
                      <Button loading={endpointLoading} onClick={() => void fetchEndpoint()}>
                        {t('sandboxDetail.getEndpoint')}
                      </Button>
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
            label: t('sandboxDetail.runtimeLogs'),
            children: (
              <Card title={t('common.logs')}>
                <Tabs
                  items={[
                    {
                      key: 'live',
                      label: t('sandboxDetail.liveLifecycle'),
                      children: <SandboxLogPanel sandboxId={id} tenant={logTenant} autoLoad />,
                    },
                    {
                      key: 'archive',
                      label: t('sandboxDetail.archivedNodeAgent'),
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
        title={t('common.renew')}
        open={renewOpen}
        confirmLoading={renewLoading}
        onOk={() => void onRenew()}
        onCancel={() => setRenewOpen(false)}
      >
        <Typography.Paragraph>{t('sandboxDetail.renewDescription')}</Typography.Paragraph>
        <InputNumber
          min={1}
          value={renewHours}
          onChange={(v) => setRenewHours(v ?? 1)}
          addonAfter={t('sandboxDetail.hours')}
        />
      </Modal>
    </div>
  );
}
