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

import { Button, Form, Input, Modal, Space, Table, Typography, message } from 'antd';
import type { ColumnsType } from 'antd/es/table';
import { useEffect, useState } from 'react';

import { SandboxStateTag } from '../components/SemanticTags';
import { ApiError, adminApi, sandboxApi, snapshotApi } from '../api/client';
import type { Snapshot } from '../api/types';
import { useAuth } from '../auth/AuthContext';

export function SnapshotsPage() {
  const { user } = useAuth();
  const [loading, setLoading] = useState(false);
  const [items, setItems] = useState<Snapshot[]>([]);
  const [unavailable, setUnavailable] = useState(false);
  const [createOpen, setCreateOpen] = useState(false);
  const [adminTenant, setAdminTenant] = useState<string>();

  const load = async () => {
    if (user?.role === 'admin') {
      if (!adminTenant) {
        setItems([]);
        return;
      }
      setLoading(true);
      setUnavailable(false);
      try {
        const data = await adminApi.listSnapshots(adminTenant);
        setItems(data.items ?? []);
      } catch (e) {
        if (e instanceof ApiError && (e.status === 403 || e.status === 501)) {
          setUnavailable(true);
          setItems([]);
        } else {
          message.error(e instanceof Error ? e.message : '加载失败');
        }
      } finally {
        setLoading(false);
      }
      return;
    }

    setLoading(true);
    setUnavailable(false);
    try {
      const data = await snapshotApi.list();
      setItems(data.items ?? []);
    } catch (e) {
      if (e instanceof ApiError && (e.status === 403 || e.status === 501)) {
        setUnavailable(true);
        setItems([]);
      } else {
        message.error(e instanceof Error ? e.message : '加载失败');
      }
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    void load();
  }, [user?.role, adminTenant]);

  const onDelete = (id: string) => {
    Modal.confirm({
      title: '删除快照？',
      okType: 'danger',
      onOk: async () => {
        try {
          if (user?.role === 'admin' && adminTenant) {
            await adminApi.deleteSnapshot(id, adminTenant);
          } else {
            await snapshotApi.remove(id);
          }
          message.success('已删除');
          await load();
        } catch (e) {
          message.error(e instanceof Error ? e.message : '删除失败');
        }
      },
    });
  };

  const onCreate = async (values: { sandboxId: string; name?: string }) => {
    try {
      if (user?.role === 'admin') {
        if (!adminTenant) {
          message.warning('请选择租户');
          return;
        }
        await adminApi.createSnapshot(values.sandboxId.trim(), adminTenant, values.name ? { name: values.name } : undefined);
      } else {
        await sandboxApi.createSnapshot(values.sandboxId.trim(), values.name ? { name: values.name } : undefined);
      }
      message.success('快照创建已提交');
      setCreateOpen(false);
      await load();
    } catch (e) {
      message.error(e instanceof Error ? e.message : '创建失败');
    }
  };

  const columns: ColumnsType<Snapshot> = [
    { title: 'ID', dataIndex: 'id' },
    { title: '名称', dataIndex: 'name', render: (v) => v ?? '—' },
    { title: '来源沙箱', dataIndex: 'sourceSandboxId' },
    {
      title: '状态',
      render: (_, r) => <SandboxStateTag state={r.status?.state} />,
    },
    { title: '创建时间', dataIndex: 'createdAt' },
    {
      title: '操作',
      render: (_, r) => (
        <Button danger size="small" onClick={() => onDelete(r.id)}>
          删除
        </Button>
      ),
    },
  ];

  return (
    <div>
      <Space style={{ marginBottom: 16, width: '100%', justifyContent: 'space-between' }}>
        <Typography.Title level={4} style={{ margin: 0 }}>
          快照
        </Typography.Title>
        <Space>
          {user?.role === 'admin' && (
            <Input
              placeholder="tenant 名称"
              style={{ width: 160 }}
              value={adminTenant}
              onChange={(e) => setAdminTenant(e.target.value || undefined)}
            />
          )}
          {(user?.role === 'tenant' || (user?.role === 'admin' && adminTenant)) && (
            <Button type="primary" onClick={() => setCreateOpen(true)}>
              从沙箱创建
            </Button>
          )}
        </Space>
      </Space>

      {user?.role === 'admin' && !adminTenant && (
        <Typography.Paragraph type="secondary">Admin 请先选择租户以代发 Lifecycle Key。</Typography.Paragraph>
      )}

      {unavailable && (
        <Typography.Paragraph type="secondary">
          快照 API 不可用或未对当前 runtime 启用。
        </Typography.Paragraph>
      )}

      <Table rowKey="id" loading={loading} columns={columns} dataSource={items} pagination={false} />

      <Modal title="从沙箱创建快照" open={createOpen} onCancel={() => setCreateOpen(false)} footer={null}>
        <Form layout="vertical" onFinish={onCreate}>
          <Form.Item name="sandboxId" label="Sandbox ID" rules={[{ required: true }]}>
            <Input />
          </Form.Item>
          <Form.Item name="name" label="名称（可选）">
            <Input />
          </Form.Item>
          <Button type="primary" htmlType="submit" block>
            创建
          </Button>
        </Form>
      </Modal>
    </div>
  );
}
