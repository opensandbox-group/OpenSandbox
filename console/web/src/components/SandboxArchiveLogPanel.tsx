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
import { useCallback, useEffect, useState } from 'react';

import { adminApi, sandboxApi } from '../api/client';
import type { DiagnosticContentResponse } from '../api/types';
import { useAuth } from '../auth/AuthContext';
import { SandboxColoredLogViewer } from './SandboxColoredLogViewer';

type Props = {
  sandboxId: string;
  tenant?: string;
  autoLoad?: boolean;
  maxHeight?: number;
};

export function SandboxArchiveLogPanel({
  sandboxId,
  tenant,
  autoLoad = true,
  maxHeight = 420,
}: Props) {
  const { user } = useAuth();
  const [data, setData] = useState<DiagnosticContentResponse | null>(null);
  const [loading, setLoading] = useState(false);
  const [disabledNote, setDisabledNote] = useState<string | null>(null);

  const fetchArchive = useCallback(async () => {
    if (!sandboxId.trim()) return;
    if (user?.role === 'admin' && !tenant) {
      message.warning('Admin 查看归档日志需指定 tenant');
      return;
    }
    setLoading(true);
    setDisabledNote(null);
    try {
      const result =
        user?.role === 'admin' && tenant
          ? await adminApi.archiveLogs(sandboxId.trim(), tenant)
          : await sandboxApi.archiveLogs(sandboxId.trim());
      setData(result);
    } catch (e) {
      setData(null);
      const msg = e instanceof Error ? e.message : '加载归档失败';
      if (msg.includes('NODEAGENT') || msg.includes('DISABLED') || msg.includes('OSS')) {
        setDisabledNote(msg);
      } else {
        message.error(msg);
      }
    } finally {
      setLoading(false);
    }
  }, [sandboxId, tenant, user?.role]);

  useEffect(() => {
    if (autoLoad && sandboxId) void fetchArchive();
  }, [autoLoad, sandboxId, fetchArchive]);

  if (disabledNote) {
    return (
      <Alert
        type="info"
        showIcon
        message="归档日志未启用"
        description={
          <>
            {disabledNote}
            <Typography.Paragraph type="secondary" style={{ marginTop: 8, marginBottom: 0 }}>
              需在集群启用 node-agent 且 BFF 配置 OSS 只读凭证（file sink 无法被 BFF 统一读取）。当前仍可使用「实时日志」Tab。
            </Typography.Paragraph>
          </>
        }
      />
    );
  }

  return (
    <div>
      <Space style={{ marginBottom: 12 }}>
        <Button type="primary" loading={loading} onClick={() => void fetchArchive()}>
          刷新归档
        </Button>
        {data?.archive?.objectKey && (
          <Typography.Text type="secondary" copyable={{ text: data.archive.objectKey }}>
            OSS: {data.archive.objectKey}
          </Typography.Text>
        )}
      </Space>
      {data?.warnings?.map((w) => (
        <Alert key={w} type="info" showIcon message={w} style={{ marginBottom: 8 }} />
      ))}
      {data?.truncated && (
        <Alert
          type="warning"
          showIcon
          style={{ marginBottom: 8 }}
          message="内容已按 maxBytes 截断，请在 OSS 下载完整对象"
        />
      )}
      {!data && !loading && (
        <Typography.Text type="secondary">点击「刷新归档」从 node-agent 持久化存储加载</Typography.Text>
      )}
      {data?.content != null && (
        <SandboxColoredLogViewer
          content={data.content}
          maxHeight={maxHeight}
          contentKey={`archive-${data.content.length}-${loading ? 'loading' : 'idle'}`}
        />
      )}
    </div>
  );
}
