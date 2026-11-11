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
import type { PoolItem, PoolUpsertRequest } from '../api/types';
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
        setError('The Pool API is unavailable in the current runtime');
      } else {
        setError(e instanceof Error ? e.message : 'Failed to load');
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
    return <Typography.Text type="secondary">The Pools page is only available to admins.</Typography.Text>;
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
      message.error(e instanceof Error ? e.message : 'Failed to load pools');
    }
  };

  const onSubmit = async () => {
    let parsed: unknown;
    try {
      parsed = JSON.parse(jsonBody);
    } catch {
      message.error('Invalid JSON format');
      return;
    }
    if (parsed === null || typeof parsed !== 'object' || Array.isArray(parsed)) {
      message.error('JSON must be an object');
      return;
    }
    const body = parsed as PoolUpsertRequest;
    if (!editing && (typeof body.name !== 'string' || !body.name)) {
      message.error('The body must include name');
      return;
    }
    try {
      if (editing) {
        await poolApi.update(editing.name, body);
        message.success('Updated');
      } else {
        await poolApi.create(body);
        message.success('Created');
      }
      setModalOpen(false);
      await load();
    } catch (e) {
      message.error(e instanceof Error ? e.message : 'Failed to submit');
    }
  };

  const onDelete = (name: string) => {
    Modal.confirm({
      title: `Delete pool ${name}?`,
      okType: 'danger',
      onOk: async () => {
        try {
          await poolApi.remove(name);
          message.success('Deleted');
          await load();
        } catch (e) {
          message.error(e instanceof Error ? e.message : 'Failed to delete');
        }
      },
    });
  };

  const columns: ColumnsType<PoolItem> = [
    { title: 'Name', dataIndex: 'name' },
    { title: 'Namespace', dataIndex: 'namespace', render: (v) => v ?? '—' },
    {
      title: 'Status',
      render: (_, r) => <SandboxStateTag state={r.status?.state} />,
    },
    {
      title: 'Actions',
      render: (_, r) => (
        <Space>
          <Button size="small" onClick={() => void openEdit(r.name)}>
            Edit
          </Button>
          <Button danger size="small" onClick={() => onDelete(r.name)}>
            Delete
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
          Create Pool
        </Button>
      </Space>
      <Alert
        type="warning"
        showIcon
        style={{ marginBottom: 16 }}
        message="Platform-level API; not tenant-isolated"
        description="As noted in tenants-configmap, /v1/pools targets the server’s default namespace."
      />
      {error && <Alert type="info" message={error} style={{ marginBottom: 16 }} />}
      <Table rowKey="name" loading={loading} columns={columns} dataSource={items} pagination={false} />

      <Modal
        title={editing ? `Edit Pool ${editing.name}` : 'Create Pool'}
        open={modalOpen}
        onOk={() => void onSubmit()}
        onCancel={() => setModalOpen(false)}
        width={720}
      >
        <Typography.Paragraph type="secondary">
          Submit Lifecycle Pool JSON in the format defined by the API. When editing, the GET response is authoritative.
        </Typography.Paragraph>
        <Input.TextArea rows={16} value={jsonBody} onChange={(e) => setJsonBody(e.target.value)} />
      </Modal>
    </div>
  );
}
