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

import { Alert, Button, Select, Space, Switch, Typography, message } from 'antd';
import { useCallback, useEffect, useRef, useState } from 'react';

import { adminApi, sandboxApi } from '../api/client';
import type { DiagnosticContentResponse } from '../api/types';
import { useAuth } from '../auth/AuthContext';
import { SandboxColoredLogViewer } from './SandboxColoredLogViewer';
import { DIAGNOSTIC_SCOPES } from '../utils/format';

type Props = {
  sandboxId: string;
  /** Admin 全局列表需指定租户，用于 BFF 代发 Key */
  tenant?: string;
  defaultScope?: string;
  autoLoad?: boolean;
  maxHeight?: number;
};

export function SandboxLogPanel({
  sandboxId,
  tenant,
  defaultScope = 'container',
  autoLoad = true,
  maxHeight = 420,
}: Props) {
  const { user } = useAuth();
  const [scope, setScope] = useState(defaultScope);
  const [data, setData] = useState<DiagnosticContentResponse | null>(null);
  const [loading, setLoading] = useState(false);
  const [autoRefresh, setAutoRefresh] = useState(false);
  const timerRef = useRef<number | null>(null);

  const fetchLogs = useCallback(async () => {
    if (!sandboxId.trim()) return;
    if (user?.role === 'admin' && !tenant) {
      message.warning('Admin 查看日志需指定 tenant');
      return;
    }
    setLoading(true);
    try {
      const result =
        user?.role === 'admin' && tenant
          ? await adminApi.diagnosticLogs(sandboxId.trim(), tenant, scope)
          : await sandboxApi.diagnosticLogs(sandboxId.trim(), scope);
      setData(result);
    } catch (e) {
      setData(null);
      message.error(e instanceof Error ? e.message : '加载日志失败');
    } finally {
      setLoading(false);
    }
  }, [sandboxId, scope, tenant, user?.role]);

  useEffect(() => {
    if (autoLoad && sandboxId) void fetchLogs();
  }, [autoLoad, sandboxId, scope, fetchLogs]);

  useEffect(() => {
    if (!autoRefresh) {
      if (timerRef.current) window.clearInterval(timerRef.current);
      timerRef.current = null;
      return;
    }
    timerRef.current = window.setInterval(() => void fetchLogs(), 10_000);
    return () => {
      if (timerRef.current) window.clearInterval(timerRef.current);
    };
  }, [autoRefresh, fetchLogs]);

  const body = () => {
    if (!data) {
      return <Typography.Text type="secondary">点击「刷新」加载日志</Typography.Text>;
    }
    if (data.contentUrl) {
      return (
        <Space direction="vertical">
          <Alert type="info" showIcon message="日志过大，请下载查看" />
          <Typography.Link href={data.contentUrl} target="_blank" rel="noreferrer">
            下载日志 ({data.contentLength ?? '?'} bytes)
          </Typography.Link>
        </Space>
      );
    }
    return (
      <SandboxColoredLogViewer
        content={data.content ?? ''}
        maxHeight={maxHeight}
        contentKey={`${scope}-${data.content?.length ?? 0}-${loading ? 'loading' : 'idle'}`}
      />
    );
  };

  return (
    <div>
      <Space wrap style={{ marginBottom: 12 }}>
        <Select
          value={scope}
          onChange={setScope}
          style={{ width: 160 }}
          options={DIAGNOSTIC_SCOPES.map((s) => ({ label: s, value: s }))}
        />
        <Button type="primary" loading={loading} onClick={() => void fetchLogs()}>
          刷新
        </Button>
        <Space>
          <Typography.Text type="secondary">自动刷新 10s</Typography.Text>
          <Switch checked={autoRefresh} onChange={setAutoRefresh} />
        </Space>
      </Space>
      {data?.truncated && (
        <Alert
          type="warning"
          showIcon
          style={{ marginBottom: 8 }}
          message="返回内容已被服务端截断，完整日志请使用 CLI 或增大 retention 配置。"
        />
      )}
      {data?.warnings?.map((w) => (
        <Alert key={w} type="warning" showIcon message={w} style={{ marginBottom: 8 }} />
      ))}
      {body()}
    </div>
  );
}
