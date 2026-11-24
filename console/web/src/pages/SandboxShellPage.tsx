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

import { FitAddon } from '@xterm/addon-fit';
import { Terminal } from '@xterm/xterm';
import '@xterm/xterm/css/xterm.css';
import { Alert, Button, Space, Typography } from 'antd';
import { useEffect, useRef, useState } from 'react';
import { Link, useParams, useSearchParams } from 'react-router-dom';

import { useAuth } from '../auth/AuthContext';
import { decodePtyOutput, encodeStdin } from '../utils/ptyFrames';

type ShellStatus = 'connecting' | 'open' | 'closed' | 'error';

const SHELL_STATUS_LABELS: Record<ShellStatus, string> = {
  connecting: 'Connecting',
  open: 'Connected',
  error: 'Failed',
  closed: 'Disconnected',
};

function shellWebSocketUrl(id: string, admin: boolean, tenant: string): string {
  const proto = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
  const path = admin
    ? `/api/admin/sandboxes/${encodeURIComponent(id)}/shell/ws?tenant=${encodeURIComponent(tenant)}`
    : `/api/sandboxes/${encodeURIComponent(id)}/shell/ws`;
  return `${proto}//${window.location.host}${path}`;
}

export function SandboxShellPage() {
  const { id = '' } = useParams<{ id: string }>();
  const [searchParams] = useSearchParams();
  const { user } = useAuth();
  const tenant = searchParams.get('tenant') ?? '';
  const isAdmin = user?.role === 'admin';
  const containerRef = useRef<HTMLDivElement>(null);
  const [attempt, setAttempt] = useState(0);
  const [status, setStatus] = useState<ShellStatus>('connecting');
  const [error, setError] = useState<string | null>(null);

  const missingTenant = isAdmin && !tenant;
  const backTo = isAdmin
    ? `/sandboxes/${encodeURIComponent(id)}?tenant=${encodeURIComponent(tenant)}`
    : `/sandboxes/${encodeURIComponent(id)}`;

  useEffect(() => {
    if (!id || missingTenant || !containerRef.current) return;
    const host = containerRef.current;
    let alive = true;
    const term = new Terminal({
      cursorBlink: true,
      fontSize: 14,
      theme: { background: '#1e1e1e', foreground: '#d4d4d4' },
    });
    const fit = new FitAddon();
    term.loadAddon(fit);
    term.open(host);
    fit.fit();

    const ws = new WebSocket(shellWebSocketUrl(id, Boolean(isAdmin), tenant));
    ws.binaryType = 'arraybuffer';

    const sendResize = () => {
      if (ws.readyState !== WebSocket.OPEN) return;
      ws.send(JSON.stringify({ type: 'resize', cols: term.cols, rows: term.rows }));
    };

    const observer = new ResizeObserver(() => {
      fit.fit();
      sendResize();
    });
    observer.observe(host);

    term.onData((data) => {
      if (ws.readyState === WebSocket.OPEN) ws.send(encodeStdin(data));
    });

    ws.onopen = () => {
      if (!alive) return;
      fit.fit();
    };

    ws.onmessage = (event) => {
      if (!alive) return;
      if (typeof event.data === 'string') {
        let frame: { type?: string; error?: string; exit_code?: number };
        try {
          frame = JSON.parse(event.data) as { type?: string; error?: string; exit_code?: number };
        } catch {
          term.write(event.data);
          return;
        }
        if (frame.type === 'connected') {
          setStatus('open');
          setError(null);
          fit.fit();
          sendResize();
          term.focus();
          return;
        }
        if (frame.type === 'error') {
          setStatus('error');
          setError(frame.error ?? 'Failed to open shell');
          return;
        }
        if (frame.type === 'exit') {
          term.write(`\r\n[process exited: ${frame.exit_code ?? '?'}]\r\n`);
          setStatus('closed');
        }
        return;
      }
      const output = decodePtyOutput(new Uint8Array(event.data as ArrayBuffer));
      if (output && output.length > 0) term.write(output);
    };

    ws.onerror = () => {
      if (!alive) return;
      setStatus('error');
      setError('Shell connection failed');
    };

    ws.onclose = () => {
      if (!alive) return;
      setStatus((current) => (current === 'error' ? current : 'closed'));
    };

    return () => {
      alive = false;
      observer.disconnect();
      ws.close();
      term.dispose();
    };
  }, [id, isAdmin, tenant, missingTenant, attempt]);

  return (
    <div>
      <Space style={{ marginBottom: 12, width: '100%', justifyContent: 'space-between' }}>
        <Space>
          <Typography.Title level={4} style={{ margin: 0 }}>
            Shell
          </Typography.Title>
          <Typography.Text type="secondary">{id}</Typography.Text>
          <Typography.Text type="secondary">{SHELL_STATUS_LABELS[status]}</Typography.Text>
        </Space>
        <Space>
          <Link to={backTo}>Back to Sandbox</Link>
          <Button
            disabled={missingTenant}
            onClick={() => {
              setError(null);
              setStatus('connecting');
              setAttempt((value) => value + 1);
            }}
          >
            Reconnect
          </Button>
        </Space>
      </Space>
      {missingTenant && (
        <Alert
          type="warning"
          showIcon
          style={{ marginBottom: 12 }}
          message="Admins must specify a tenant to open a shell"
          description="Open this page from the All Tenants sandbox list, or visit /sandboxes/{id}/shell?tenant=tenant-name"
        />
      )}
      {error && <Alert type="error" showIcon style={{ marginBottom: 12 }} message={error} />}
      <div
        ref={containerRef}
        style={{
          height: 'calc(100vh - 180px)',
          minHeight: 320,
          background: '#1e1e1e',
          borderRadius: 8,
          padding: 8,
          display: missingTenant ? 'none' : 'block',
        }}
      />
    </div>
  );
}
