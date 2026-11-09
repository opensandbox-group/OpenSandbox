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
  /** The admin global list requires a tenant so the BFF can forward its key. */
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
  const [contentVersion, setContentVersion] = useState(0);
  const timerRef = useRef<number | null>(null);
  const requestSeq = useRef(0);

  const fetchLogs = useCallback(async (notify: boolean) => {
    if (!sandboxId.trim()) return;
    if (user?.role === 'admin' && !tenant) {
      if (notify) message.warning('Admins must specify a tenant to view logs');
      return;
    }
    const seq = ++requestSeq.current;
    setLoading(true);
    try {
      const result =
        user?.role === 'admin' && tenant
          ? await adminApi.diagnosticLogs(sandboxId.trim(), tenant, scope)
          : await sandboxApi.diagnosticLogs(sandboxId.trim(), scope);
      if (seq !== requestSeq.current) return;
      setData(result);
      setContentVersion((version) => version + 1);
    } catch (e) {
      if (seq !== requestSeq.current) return;
      if (notify) {
        setData(null);
        message.error(e instanceof Error ? e.message : 'Failed to load logs');
      }
    } finally {
      if (seq === requestSeq.current) setLoading(false);
    }
  }, [sandboxId, scope, tenant, user?.role]);

  useEffect(() => {
    if (autoLoad && sandboxId) void fetchLogs(true);
  }, [autoLoad, sandboxId, scope, fetchLogs]);

  useEffect(() => {
    if (!autoRefresh) {
      if (timerRef.current) window.clearInterval(timerRef.current);
      timerRef.current = null;
      return;
    }
    timerRef.current = window.setInterval(() => void fetchLogs(false), 10_000);
    return () => {
      if (timerRef.current) window.clearInterval(timerRef.current);
    };
  }, [autoRefresh, fetchLogs]);

  const body = () => {
    if (!data) {
      return <Typography.Text type="secondary">Select “Refresh” to load logs</Typography.Text>;
    }
    if (data.contentUrl) {
      return (
        <Space direction="vertical">
          <Alert type="info" showIcon message="Log is too large. Download it to view." />
          <Typography.Link href={data.contentUrl} target="_blank" rel="noreferrer">
            Download logs ({data.contentLength ?? '?'} bytes)
          </Typography.Link>
        </Space>
      );
    }
    return (
      <SandboxColoredLogViewer
        content={data.content ?? ''}
        maxHeight={maxHeight}
        contentKey={`${scope}-${contentVersion}`}
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
        <Button type="primary" loading={loading} onClick={() => void fetchLogs(true)}>
          Refresh
        </Button>
        <Space>
          <Typography.Text type="secondary">Auto-refresh: 10s</Typography.Text>
          <Switch checked={autoRefresh} onChange={setAutoRefresh} />
        </Space>
      </Space>
      {data?.truncated && (
        <Alert
          type="warning"
          showIcon
          style={{ marginBottom: 8 }}
          message="The server truncated the response. Use the CLI or increase the retention setting to retrieve the full log."
        />
      )}
      {data?.warnings?.map((w) => (
        <Alert key={w} type="warning" showIcon message={w} style={{ marginBottom: 8 }} />
      ))}
      {body()}
    </div>
  );
}
