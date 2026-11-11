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

import { Alert, Input, Select, Space, Tabs, Typography, message } from 'antd';
import { useEffect, useRef, useState } from 'react';
import { useSearchParams } from 'react-router-dom';

import { ApiError, adminApi, sandboxApi } from '../api/client';
import type { DiagnosticContentResponse } from '../api/types';
import { SandboxArchiveLogPanel } from '../components/SandboxArchiveLogPanel';
import { SandboxLogPanel } from '../components/SandboxLogPanel';
import { useAuth } from '../auth/AuthContext';
import { DIAGNOSTIC_SCOPES } from '../utils/format';

export function DiagnosticsPage() {
  const { user } = useAuth();
  const [searchParams, setSearchParams] = useSearchParams();
  const [sandboxId, setSandboxId] = useState(searchParams.get('sandboxId') ?? '');
  const [tenant, setTenant] = useState(searchParams.get('tenant') ?? '');
  const [scope, setScope] = useState('runtime');
  const [events, setEvents] = useState<DiagnosticContentResponse | null>(null);
  const [loadingEvents, setLoadingEvents] = useState(false);
  const requestSeq = useRef(0);

  useEffect(() => {
    requestSeq.current += 1;
    setEvents(null);
    setLoadingEvents(false);
  }, [sandboxId, tenant, scope]);

  useEffect(() => {
    const id = searchParams.get('sandboxId');
    const t = searchParams.get('tenant');
    if (id) setSandboxId(id);
    if (t) setTenant(t);
  }, [searchParams]);

  const syncUrl = (id: string, t: string) => {
    const next = new URLSearchParams();
    if (id) next.set('sandboxId', id);
    if (t) next.set('tenant', t);
    setSearchParams(next, { replace: true });
  };

  const renderPayload = (data: DiagnosticContentResponse | null) => {
    if (!data) return <Typography.Text type="secondary">No data available</Typography.Text>;
    if (data.contentUrl) {
      return (
        <Typography.Link href={data.contentUrl} target="_blank" rel="noreferrer">
          Download diagnostic data ({data.contentLength ?? 'unknown'} bytes)
        </Typography.Link>
      );
    }
    return (
      <pre style={{ background: '#1e1e1e', color: '#d4d4d4', padding: 12, maxHeight: 480, overflow: 'auto' }}>
        {data.content ?? JSON.stringify(data, null, 2)}
      </pre>
    );
  };

  const fetchEvents = async () => {
    if (!sandboxId.trim()) {
      message.warning('Enter a Sandbox ID');
      return;
    }
    if (user?.role === 'admin' && !tenant.trim()) {
      message.warning('Admins must specify a tenant to view events');
      setEvents(null);
      return;
    }
    const seq = ++requestSeq.current;
    setLoadingEvents(true);
    try {
      const data =
        user?.role === 'admin'
          ? await adminApi.diagnosticEvents(sandboxId.trim(), tenant.trim(), scope)
          : await sandboxApi.diagnosticEvents(sandboxId.trim(), scope);
      if (seq !== requestSeq.current) return;
      setEvents(data);
    } catch (e) {
      if (seq !== requestSeq.current) return;
      if (e instanceof ApiError && e.status === 404) {
        message.info('The diagnostics API returned no data');
      } else {
        message.error(e instanceof Error ? e.message : 'Request failed');
      }
      setEvents(null);
    } finally {
      if (seq === requestSeq.current) setLoadingEvents(false);
    }
  };

  return (
    <div>
      <Typography.Title level={4}>Diagnostics</Typography.Title>
      {user?.role === 'admin' && (
        <Alert
          type="info"
          showIcon
          style={{ marginBottom: 16 }}
          message="Admins must enter a tenant to view logs or events, as in the global sandbox list. The BFF will make the request using that tenant’s key."
        />
      )}
      <Space wrap style={{ marginBottom: 16 }}>
        <Input
          placeholder="Sandbox ID"
          value={sandboxId}
          onChange={(e) => setSandboxId(e.target.value)}
          onBlur={() => syncUrl(sandboxId, tenant)}
          style={{ width: 280 }}
        />
        {user?.role === 'admin' && (
          <Input
            placeholder="Tenant name"
            value={tenant}
            onChange={(e) => setTenant(e.target.value)}
            onBlur={() => syncUrl(sandboxId, tenant)}
            style={{ width: 160 }}
          />
        )}
      </Space>
      <Tabs
        items={[
          {
            key: 'logs',
            label: 'Runtime Logs',
            children: sandboxId.trim() ? (
              <Tabs
                items={[
                  {
                    key: 'live',
                    label: 'Live',
                    children: (
                      <SandboxLogPanel
                        sandboxId={sandboxId.trim()}
                        tenant={user?.role === 'admin' ? tenant.trim() : undefined}
                        autoLoad={user?.role === 'tenant' || Boolean(tenant.trim())}
                      />
                    ),
                  },
                  {
                    key: 'archive',
                    label: 'Archived',
                    children: (
                      <SandboxArchiveLogPanel
                        sandboxId={sandboxId.trim()}
                        tenant={user?.role === 'admin' ? tenant.trim() : undefined}
                        autoLoad={user?.role === 'tenant' || Boolean(tenant.trim())}
                      />
                    ),
                  },
                ]}
              />
            ) : (
              <Typography.Text type="secondary">Enter a Sandbox ID first</Typography.Text>
            ),
          },
          {
            key: 'events',
            label: 'Events',
            children: (
              <>
                <Space style={{ marginBottom: 8 }}>
                  <Select
                    value={scope}
                    onChange={setScope}
                    style={{ width: 160 }}
                    options={DIAGNOSTIC_SCOPES.map((s) => ({ label: s, value: s }))}
                  />
                  <Typography.Link onClick={() => void fetchEvents()}>Load</Typography.Link>
                  {loadingEvents && <Typography.Text type="secondary">Loading…</Typography.Text>}
                </Space>
                {renderPayload(events)}
              </>
            ),
          },
        ]}
      />
    </div>
  );
}
