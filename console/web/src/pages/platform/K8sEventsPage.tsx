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

import { Alert, Input, Space, Table, Typography } from 'antd';
import type { ColumnsType } from 'antd/es/table';
import { useEffect, useState } from 'react';

import { adminApi } from '../../api/client';
import type { K8sEventRow } from '../../api/types';
import { useAuth } from '../../auth/AuthContext';

export function K8sEventsPage() {
  const { user } = useAuth();
  const [loading, setLoading] = useState(false);
  const [items, setItems] = useState<K8sEventRow[]>([]);
  const [disabledMsg, setDisabledMsg] = useState<string | null>(null);
  const [sandboxId, setSandboxId] = useState('');

  const load = async () => {
    if (user?.role !== 'admin') return;
    setLoading(true);
    setDisabledMsg(null);
    try {
      const data = await adminApi.k8sEvents({
        sandboxId: sandboxId.trim() || undefined,
      });
      if (data.disabled) {
        setDisabledMsg(data.message ?? 'K8s API 未启用');
        setItems([]);
      } else {
        setItems(data.items ?? []);
      }
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    void load();
  }, [user?.role]);

  if (user?.role !== 'admin') {
    return <Typography.Text type="secondary">仅 Admin 可查看 K8s 事件。</Typography.Text>;
  }

  const columns: ColumnsType<K8sEventRow> = [
    { title: 'Namespace', dataIndex: 'namespace', width: 120 },
    { title: 'Type', dataIndex: 'type', width: 80 },
    { title: 'Reason', dataIndex: 'reason', width: 140 },
    { title: 'Object', render: (_, r) => `${r.involvedObject?.kind ?? ''}/${r.involvedObject?.name ?? ''}` },
    { title: 'Message', dataIndex: 'message', ellipsis: true },
    { title: 'Last', dataIndex: 'lastTimestamp', width: 180 },
  ];

  return (
    <div>
      <Typography.Title level={4}>K8s 事件</Typography.Title>
      {disabledMsg && (
        <Alert type="info" showIcon message={disabledMsg} style={{ marginBottom: 16 }} />
      )}
      <Space wrap style={{ marginBottom: 16 }}>
        <Input
          placeholder="按 Sandbox ID 过滤 Pod 事件"
          value={sandboxId}
          onChange={(e) => setSandboxId(e.target.value)}
          style={{ width: 260 }}
        />
        <Typography.Link onClick={() => void load()}>刷新</Typography.Link>
      </Space>
      <Table rowKey={(r, i) => `${r.namespace}-${r.lastTimestamp}-${i}`} loading={loading} columns={columns} dataSource={items} />
    </div>
  );
}
