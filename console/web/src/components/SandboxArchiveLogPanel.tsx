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

import { Alert, Button, Space, Typography, message } from 'antd';
import { useCallback, useEffect, useRef, useState } from 'react';

import { ApiError, adminApi, sandboxApi } from '../api/client';
import type { DiagnosticContentResponse } from '../api/types';
import { useAuth } from '../auth/AuthContext';
import { SandboxColoredLogViewer } from './SandboxColoredLogViewer';

type Props = {
  sandboxId: string;
  tenant?: string;
  autoLoad?: boolean;
  maxHeight?: number;
};

const ARCHIVE_DISABLED_CODES = new Set([
  'NODEAGENT_ARCHIVE_DISABLED',
  'NODEAGENT_OSS_NOT_CONFIGURED',
]);

export function SandboxArchiveLogPanel({
  sandboxId,
  tenant,
  autoLoad = true,
  maxHeight = 420,
}: Props) {
  const { user } = useAuth();
  const requestSeq = useRef(0);
  const [contentVersion, setContentVersion] = useState(0);
  const [data, setData] = useState<DiagnosticContentResponse | null>(null);
  const [loading, setLoading] = useState(false);
  const [disabledNote, setDisabledNote] = useState<string | null>(null);

  const fetchArchive = useCallback(async () => {
    if (!sandboxId.trim()) return;
    if (user?.role === 'admin' && !tenant) {
      message.warning('Admins must specify a tenant to view archived logs');
      return;
    }
    const seq = ++requestSeq.current;
    setLoading(true);
    setDisabledNote(null);
    try {
      const result =
        user?.role === 'admin' && tenant
          ? await adminApi.archiveLogs(sandboxId.trim(), tenant)
          : await sandboxApi.archiveLogs(sandboxId.trim());
      if (seq !== requestSeq.current) return;
      setData(result);
      setContentVersion((v) => v + 1);
    } catch (e) {
      if (seq !== requestSeq.current) return;
      setData(null);
      if (e instanceof ApiError && ARCHIVE_DISABLED_CODES.has(e.code)) {
        setDisabledNote(e.message);
      } else {
        message.error(e instanceof Error ? e.message : 'Failed to load archived logs');
      }
    } finally {
      if (seq === requestSeq.current) setLoading(false);
    }
  }, [sandboxId, tenant, user?.role]);

  useEffect(() => {
    if (autoLoad && sandboxId) void fetchArchive();
  }, [autoLoad, sandboxId, tenant, fetchArchive]);

  if (disabledNote) {
    return (
      <Alert
        type="info"
        showIcon
        message="Archived logs are not enabled"
        description={
          <>
            {disabledNote}
            <Typography.Paragraph type="secondary" style={{ marginTop: 8, marginBottom: 0 }}>
              Enable node-agent in the cluster and configure read-only OSS credentials in the BFF to use archived logs. The BFF cannot centrally read file sinks. You can still use the Live Logs tab.
            </Typography.Paragraph>
          </>
        }
        action={
          <Button size="small" onClick={() => void fetchArchive()}>
            Retry
          </Button>
        }
      />
    );
  }

  return (
    <div>
      <Space style={{ marginBottom: 12 }}>
        <Button type="primary" loading={loading} onClick={() => void fetchArchive()}>
          Refresh Archive
        </Button>
        {data?.archive?.objectKey && (
          <Typography.Text type="secondary" copyable={{ text: data.archive.objectKey }}>
            OSS: {data.archive.objectKey}
          </Typography.Text>
        )}
      </Space>
      {data?.warnings?.map((w, i) => (
        <Alert key={`${i}-${w.slice(0, 40)}`} type="info" showIcon message={w} style={{ marginBottom: 8 }} />
      ))}
      {data?.truncated && (
        <Alert
          type="warning"
          showIcon
          style={{ marginBottom: 8 }}
          message="Content was truncated to maxBytes. Download the complete object from OSS."
        />
      )}
      {!data && !loading && (
        <Typography.Text type="secondary">Select “Refresh Archive” to load logs from node-agent storage</Typography.Text>
      )}
      {data?.content != null && (
        <SandboxColoredLogViewer
          content={data.content}
          maxHeight={maxHeight}
          contentKey={`archive-${contentVersion}`}
        />
      )}
    </div>
  );
}
