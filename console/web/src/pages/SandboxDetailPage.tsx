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

import { adminApi, ApiError, historyApi, sandboxApi } from '../api/client';
import { SandboxArchiveLogPanel } from '../components/SandboxArchiveLogPanel';
import { SandboxLogPanel } from '../components/SandboxLogPanel';
import { SandboxStateTag, TenantTag } from '../components/SemanticTags';
import type { Sandbox, SandboxHistoryItem } from '../api/types';
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
  const [historyRecord, setHistoryRecord] = useState<SandboxHistoryItem | null>(null);
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

  const loadHistoryRecord = useCallback(async () => {
    if (!id) return null;
    const tenant = isAdminProxy ? adminTenant : undefined;
    try {
      return await historyApi.getSandbox(id, tenant);
    } catch (e) {
      if (e instanceof ApiError && e.status === 404) return null;
      throw e;
    }
  }, [adminTenant, id, isAdminProxy]);

  const load = useCallback(async () => {
    if (!api) return;
    setLoading(true);
    setHistoryRecord(null);
    try {
      const sb = await api.get();
      setSandbox(sb);
      try {
        setHistoryRecord(await loadHistoryRecord());
      } catch {
        /* 历史未启用或查询失败时不阻塞详情 */
      }
    } catch (e) {
      if (e instanceof ApiError && e.status === 404) {
        try {
          const record = await loadHistoryRecord();
          setSandbox(null);
          setHistoryRecord(record);
          return;
        } catch (he) {
          message.error(he instanceof Error ? he.message : '历史记录加载失败');
        }
      } else {
        message.error(e instanceof Error ? e.message : '加载失败');
      }
    } finally {
      setLoading(false);
    }
  }, [api, loadHistoryRecord]);

  useEffect(() => {
    if (user?.role === 'admin' && !adminTenant) {
      message.warning('Admin 查看详情需 URL 参数 tenant=');
      setLoading(false);
      return;
    }
    void load();
  }, [load, user?.role, adminTenant]);

  const state = sandbox?.status?.state;
  const historyOnly = !sandbox && historyRecord != null;

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

  const displayState = state ?? historyRecord?.state;
  const titleText = sandbox ? sandboxDisplayName(sandbox) : historyRecord?.sandboxId ?? id;
  const configSource = historyRecord;

  const renderResourceConfig = () => {
    const limits = configSource?.resourceLimits;
    if (!limits?.cpu && !limits?.memory && !limits?.disk) {
      return (
        <Typography.Text type="secondary">
          暂无记录。经 Console 创建或列表同步后会写入 PostgreSQL；SDK 直连创建可能未入库。
        </Typography.Text>
      );
    }
    return (
      <Descriptions column={1} bordered size="small">
        <Descriptions.Item label="CPU limits">{limits.cpu ?? '—'}</Descriptions.Item>
        <Descriptions.Item label="内存 limits">{limits.memory ?? '—'}</Descriptions.Item>
        {limits.disk && <Descriptions.Item label="磁盘 limits">{limits.disk}</Descriptions.Item>}
        {configSource?.cpuCores != null && (
          <Descriptions.Item label="CPU（换算核数）">{configSource.cpuCores}</Descriptions.Item>
        )}
        {configSource?.memoryGi != null && (
          <Descriptions.Item label="内存（换算 GiB）">{configSource.memoryGi}</Descriptions.Item>
        )}
        {configSource?.resourceRequests &&
          Object.keys(configSource.resourceRequests).length > 0 && (
            <Descriptions.Item label="resourceRequests">
              {Object.entries(configSource.resourceRequests)
                .map(([k, v]) => `${k}: ${v}`)
                .join(' · ')}
            </Descriptions.Item>
          )}
        {configSource?.createTimeoutSeconds != null && (
          <Descriptions.Item label="创建 timeout（秒）">
            {configSource.createTimeoutSeconds}
          </Descriptions.Item>
        )}
      </Descriptions>
    );
  };

  return (
    <div>
      <Space style={{ marginBottom: 16 }}>
        <Typography.Title level={4} style={{ margin: 0 }}>
          {titleText}
        </Typography.Title>
        {displayState && <SandboxStateTag state={displayState} />}
        {historyOnly && <Tag color="default">历史记录</Tag>}
        {isAdminProxy && <TenantTag tenant={adminTenant} />}
      </Space>

      {historyOnly && (
        <Alert
          type="info"
          showIcon
          style={{ marginBottom: 16 }}
          message="沙箱已从 Lifecycle 移除"
          description="以下为 PostgreSQL 申请历史中的持久化信息；归档日志仍可查看（若 Node Agent 已保留）。"
        />
      )}

      <Tabs
        defaultActiveKey={defaultTab}
        items={[
          {
            key: 'overview',
            label: '概览',
            children: (
              <Row gutter={[16, 16]}>
                <Col xs={24} lg={historyOnly ? 24 : 12}>
                  <Card loading={loading} title={historyOnly ? '历史状态' : '状态'}>
                    {historyOnly && historyRecord ? (
                      <Descriptions column={1} bordered size="small">
                        <Descriptions.Item label="ID">{historyRecord.sandboxId}</Descriptions.Item>
                        <Descriptions.Item label="租户">{historyRecord.tenant ?? '—'}</Descriptions.Item>
                        <Descriptions.Item label="命名空间">{historyRecord.namespace ?? '—'}</Descriptions.Item>
                        <Descriptions.Item label="状态">{historyRecord.state ?? '—'}</Descriptions.Item>
                        <Descriptions.Item label="运行时长">
                          {historyRecord.wallClockSeconds != null
                            ? formatDuration(historyRecord.wallClockSeconds)
                            : '—'}
                        </Descriptions.Item>
                        <Descriptions.Item label="创建时间">{historyRecord.createdAt ?? '—'}</Descriptions.Item>
                        <Descriptions.Item label="过期时间">{historyRecord.expiresAt ?? '—'}</Descriptions.Item>
                        <Descriptions.Item label="结束时间">{historyRecord.endedAt ?? '—'}</Descriptions.Item>
                        <Descriptions.Item label="删除时间">{historyRecord.deletedAt ?? '—'}</Descriptions.Item>
                        <Descriptions.Item label="镜像">{historyRecord.imageUri ?? '—'}</Descriptions.Item>
                        <Descriptions.Item label="快照数">{historyRecord.snapshotCount ?? 0}</Descriptions.Item>
                        <Descriptions.Item label="来源">{historyRecord.source ?? '—'}</Descriptions.Item>
                        <Descriptions.Item label="首次入库">{historyRecord.firstRecordedAt ?? '—'}</Descriptions.Item>
                        <Descriptions.Item label="最后同步">{historyRecord.lastSeenAt ?? '—'}</Descriptions.Item>
                      </Descriptions>
                    ) : (
                      <>
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
                      </>
                    )}
                  </Card>
                </Col>
                <Col xs={24} lg={historyOnly ? 24 : 12}>
                  <Card loading={loading} title="申请配置（resourceLimits）">
                    {renderResourceConfig()}
                    <Typography.Paragraph type="secondary" style={{ marginTop: 12, marginBottom: 0, fontSize: 12 }}>
                      与用量分摊口径一致：按创建时的 limits 满额 × 占用时长，非实际利用率。
                    </Typography.Paragraph>
                  </Card>
                </Col>
                {!historyOnly && (
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
                )}
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
                      disabled: historyOnly,
                      children: historyOnly ? (
                        <Typography.Text type="secondary">沙箱已销毁，无实时日志。</Typography.Text>
                      ) : (
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
