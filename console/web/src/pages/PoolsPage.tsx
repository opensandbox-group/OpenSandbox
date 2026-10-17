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

import { Alert, Button, Input, Modal, Space, Table, Typography, message } from 'antd';
import type { ColumnsType } from 'antd/es/table';
import { useEffect, useState } from 'react';

import { SandboxStateTag } from '../components/SemanticTags';
import { ApiError, poolApi } from '../api/client';
import type { PoolItem } from '../api/types';
import { useAuth } from '../auth/AuthContext';

export function PoolsPage() {
  const { user } = useAuth();
  const [loading, setLoading] = useState(false);
  const [items, setItems] = useState<PoolItem[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [modalOpen, setModalOpen] = useState(false);
  const [editing, setEditing] = useState<PoolItem | null>(null);
  const [jsonBody, setJsonBody] = useState('{\n  "name": "example-pool"\n}');

  const load = async () => {
    setLoading(true);
    setError(null);
    try {
      const data = await poolApi.list();
      setItems(data.items ?? []);
    } catch (e) {
      setItems([]);
      if (e instanceof ApiError && (e.status === 501 || e.status === 404)) {
        setError('Pool API 在当前 runtime 不可用');
      } else {
        setError(e instanceof Error ? e.message : '加载失败');
      }
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    if (user?.role !== 'admin') return;
    void load();
  }, [user?.role]);

  if (user?.role !== 'admin') {
    return <Typography.Text type="secondary">Pool 页仅 Admin 可见。</Typography.Text>;
  }

  const openCreate = () => {
    setEditing(null);
    setJsonBody('{\n  "name": "my-pool"\n}');
    setModalOpen(true);
  };

  const openEdit = async (name: string) => {
    try {
      const pool = await poolApi.get(name);
      setEditing(pool);
      setJsonBody(JSON.stringify(pool, null, 2));
      setModalOpen(true);
    } catch (e) {
      message.error(e instanceof Error ? e.message : '加载 Pool 失败');
    }
  };

  const onSubmit = async () => {
    let body: Record<string, unknown>;
    try {
      body = JSON.parse(jsonBody) as Record<string, unknown>;
    } catch {
      message.error('JSON 格式无效');
      return;
    }
    const name = String(body.name ?? editing?.name ?? '');
    if (!name) {
      message.error('body 需包含 name');
      return;
    }
    try {
      if (editing) {
        await poolApi.update(name, body);
        message.success('已更新');
      } else {
        await poolApi.create(body);
        message.success('已创建');
      }
      setModalOpen(false);
      await load();
    } catch (e) {
      message.error(e instanceof Error ? e.message : '提交失败');
    }
  };

  const onDelete = (name: string) => {
    Modal.confirm({
      title: `删除 Pool ${name}？`,
      okType: 'danger',
      onOk: async () => {
        try {
          await poolApi.remove(name);
          message.success('已删除');
          await load();
        } catch (e) {
          message.error(e instanceof Error ? e.message : '删除失败');
        }
      },
    });
  };

  const columns: ColumnsType<PoolItem> = [
    { title: '名称', dataIndex: 'name' },
    { title: 'Namespace', dataIndex: 'namespace', render: (v) => v ?? '—' },
    {
      title: '状态',
      render: (_, r) => <SandboxStateTag state={r.status?.state} />,
    },
    {
      title: '操作',
      render: (_, r) => (
        <Space>
          <Button size="small" onClick={() => void openEdit(r.name)}>
            编辑
          </Button>
          <Button danger size="small" onClick={() => onDelete(r.name)}>
            删除
          </Button>
        </Space>
      ),
    },
  ];

  return (
    <div>
      <Space style={{ marginBottom: 16, width: '100%', justifyContent: 'space-between' }}>
        <Typography.Title level={4} style={{ margin: 0 }}>
          Pool
        </Typography.Title>
        <Button type="primary" onClick={openCreate}>
          创建 Pool
        </Button>
      </Space>
      <Alert
        type="warning"
        showIcon
        style={{ marginBottom: 16 }}
        message="平台级 API，非租户隔离"
        description="与 tenants-configmap 注释一致：/v1/pools 指向 server 默认 namespace。"
      />
      {error && <Alert type="info" message={error} style={{ marginBottom: 16 }} />}
      <Table rowKey="name" loading={loading} columns={columns} dataSource={items} pagination={false} />

      <Modal
        title={editing ? `编辑 Pool ${editing.name}` : '创建 Pool'}
        open={modalOpen}
        onOk={() => void onSubmit()}
        onCancel={() => setModalOpen(false)}
        width={720}
      >
        <Typography.Paragraph type="secondary">
          提交 Lifecycle Pool JSON（与 API 契约一致）。编辑时以 GET 结果为准。
        </Typography.Paragraph>
        <Input.TextArea rows={16} value={jsonBody} onChange={(e) => setJsonBody(e.target.value)} />
      </Modal>
    </div>
  );
}
