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

import { Alert, Input, Select, Space, Table, Tag, Typography } from 'antd';
import type { ColumnsType } from 'antd/es/table';
import { useEffect, useState } from 'react';

import { TenantTag } from '../../components/SemanticTags';
import { adminApi } from '../../api/client';
import type { K8sWorkloadRow } from '../../api/types';
import { useAuth } from '../../auth/AuthContext';

export function K8sWorkloadsPage() {
  const { user } = useAuth();
  const [loading, setLoading] = useState(false);
  const [items, setItems] = useState<K8sWorkloadRow[]>([]);
  const [disabledMsg, setDisabledMsg] = useState<string | null>(null);
  const [tenant, setTenant] = useState<string>();
  const [sandboxId, setSandboxId] = useState('');

  const load = async () => {
    if (user?.role !== 'admin') return;
    setLoading(true);
    setDisabledMsg(null);
    try {
      const data = await adminApi.k8sWorkloads({
        tenant,
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
  }, [user?.role, tenant]);

  if (user?.role !== 'admin') {
    return <Typography.Text type="secondary">仅 Admin 可查看 K8s 工作负载。</Typography.Text>;
  }

  const columns: ColumnsType<K8sWorkloadRow> = [
    { title: '租户', dataIndex: 'tenant', width: 90, render: (t: string) => <TenantTag tenant={t} /> },
    { title: 'Namespace', dataIndex: 'namespace', width: 120 },
    { title: 'Pod', dataIndex: 'name', ellipsis: true },
    {
      title: 'Sandbox ID',
      dataIndex: 'sandboxId',
      render: (v) => (v ? <Tag>{v}</Tag> : '—'),
    },
    { title: 'Phase', dataIndex: 'phase', width: 100 },
    { title: 'Node', dataIndex: 'nodeName', ellipsis: true },
    { title: 'Ready', dataIndex: 'ready', width: 80 },
  ];

  return (
    <div>
      <Typography.Title level={4}>K8s 工作负载（Pod）</Typography.Title>
      {disabledMsg && (
        <Alert type="info" showIcon message={disabledMsg} style={{ marginBottom: 16 }} />
      )}
      <Space wrap style={{ marginBottom: 16 }}>
        <Input
          placeholder="Sandbox ID 筛选"
          value={sandboxId}
          onChange={(e) => setSandboxId(e.target.value)}
          style={{ width: 220 }}
        />
        <Select
          allowClear
          placeholder="租户"
          style={{ width: 140 }}
          value={tenant}
          onChange={setTenant}
          options={[...new Set(items.map((i) => i.tenant).filter(Boolean))].map((t) => ({
            label: t,
            value: t!,
          }))}
        />
        <Typography.Link onClick={() => void load()}>刷新</Typography.Link>
      </Space>
      <Table rowKey={(r) => `${r.namespace}/${r.name}`} loading={loading} columns={columns} dataSource={items} />
    </div>
  );
}
