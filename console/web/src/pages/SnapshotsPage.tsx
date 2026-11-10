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
import { useCallback, useEffect, useRef, useState } from 'react';

import { SandboxStateTag } from '../components/SemanticTags';
import { ApiError, adminApi, sandboxApi, snapshotApi } from '../api/client';
import type { Snapshot } from '../api/types';
import { useAuth } from '../auth/AuthContext';
import { formatDateTime } from '../utils/format';

function snapshotUnavailable(error: unknown): boolean {
  return error instanceof ApiError && (error.status === 403 || error.status === 404 || error.status === 501);
}

export function SnapshotsPage() {
  const { user } = useAuth();
  const [loading, setLoading] = useState(false);
  const [items, setItems] = useState<Snapshot[]>([]);
  const [unavailable, setUnavailable] = useState(false);
  const [createOpen, setCreateOpen] = useState(false);
  const [tenantInput, setTenantInput] = useState('');
  const [adminTenant, setAdminTenant] = useState<string>();
  const [page, setPage] = useState(1);
  const [pageSize, setPageSize] = useState(20);
  const [total, setTotal] = useState(0);
  const requestSeq = useRef(0);
  const appliedTenant = useRef<string | undefined>(undefined);

  useEffect(() => {
    const timer = window.setTimeout(() => {
      const next = tenantInput.trim() || undefined;
      if (appliedTenant.current === next) return;
      appliedTenant.current = next;
      setAdminTenant(next);
      setPage(1);
    }, 300);
    return () => window.clearTimeout(timer);
  }, [tenantInput]);

  const load = useCallback(async () => {
    if (user?.role === 'admin' && !adminTenant) {
      setItems([]);
      setTotal(0);
      setUnavailable(false);
      return;
    }
    const seq = ++requestSeq.current;
    setLoading(true);
    setUnavailable(false);
    try {
      const data =
        user?.role === 'admin' && adminTenant
          ? await adminApi.listSnapshots(adminTenant, { page, pageSize })
          : await snapshotApi.list({ page: String(page), pageSize: String(pageSize) });
      if (seq !== requestSeq.current) return;
      setItems(data.items ?? []);
      setTotal(data.pagination?.totalItems ?? data.items?.length ?? 0);
    } catch (e) {
      if (seq !== requestSeq.current) return;
      if (snapshotUnavailable(e)) {
        setUnavailable(true);
        setItems([]);
        setTotal(0);
      } else {
        message.error(e instanceof Error ? e.message : 'Failed to load');
      }
    } finally {
      if (seq === requestSeq.current) setLoading(false);
    }
  }, [adminTenant, page, pageSize, user?.role]);

  useEffect(() => {
    void load();
  }, [load]);

  const onDelete = (id: string) => {
    Modal.confirm({
      title: 'Delete snapshot?',
      okType: 'danger',
      onOk: async () => {
        try {
          if (user?.role === 'admin' && adminTenant) {
            await adminApi.deleteSnapshot(id, adminTenant);
          } else {
            await snapshotApi.remove(id);
          }
          message.success('Deleted');
          await load();
        } catch (e) {
          message.error(e instanceof Error ? e.message : 'Failed to delete');
        }
      },
    });
  };

  const onCreate = async (values: { sandboxId: string; name?: string }) => {
    try {
      if (user?.role === 'admin') {
        if (!adminTenant) {
          message.warning('Select a tenant');
          return;
        }
        await adminApi.createSnapshot(values.sandboxId.trim(), adminTenant, values.name ? { name: values.name } : undefined);
      } else {
        await sandboxApi.createSnapshot(values.sandboxId.trim(), values.name ? { name: values.name } : undefined);
      }
      message.success('Snapshot creation requested');
      setCreateOpen(false);
      await load();
    } catch (e) {
      message.error(e instanceof Error ? e.message : 'Failed to create');
    }
  };

  const columns: ColumnsType<Snapshot> = [
    { title: 'ID', dataIndex: 'id' },
    { title: 'Name', dataIndex: 'name', render: (v) => v ?? '—' },
    { title: 'Source Sandbox', dataIndex: 'sourceSandboxId' },
    {
      title: 'Status',
      render: (_, r) => <SandboxStateTag state={r.status?.state} />,
    },
    {
      title: 'Created At',
      dataIndex: 'createdAt',
      render: (v: string | undefined) => formatDateTime(v),
    },
    {
      title: 'Actions',
      render: (_, r) => (
        <Button danger size="small" onClick={() => onDelete(r.id)}>
          Delete
        </Button>
      ),
    },
  ];

  return (
    <div>
      <Space style={{ marginBottom: 16, width: '100%', justifyContent: 'space-between' }}>
        <Typography.Title level={4} style={{ margin: 0 }}>
          Snapshots
        </Typography.Title>
        <Space>
          {user?.role === 'admin' && (
            <Input
              placeholder="Tenant name"
              style={{ width: 160 }}
              value={tenantInput}
              onChange={(e) => setTenantInput(e.target.value)}
              onPressEnter={() => {
                const next = tenantInput.trim() || undefined;
                appliedTenant.current = next;
                setAdminTenant(next);
                setPage(1);
              }}
            />
          )}
          {(user?.role === 'tenant' || (user?.role === 'admin' && adminTenant)) && (
            <Button type="primary" onClick={() => setCreateOpen(true)}>
              Create from Sandbox
            </Button>
          )}
        </Space>
      </Space>

      {user?.role === 'admin' && !adminTenant && (
        <Typography.Paragraph type="secondary">Admins must select a tenant first so the BFF can use its Lifecycle key.</Typography.Paragraph>
      )}

      {unavailable && (
        <Typography.Paragraph type="secondary">
          The snapshot API is unavailable or not enabled for the current runtime.
        </Typography.Paragraph>
      )}

      <Table
        rowKey="id"
        loading={loading}
        columns={columns}
        dataSource={items}
        pagination={{
          current: page,
          pageSize,
          total,
          showSizeChanger: true,
          onChange: (nextPage, nextSize) => {
            setPage(nextPage);
            setPageSize(nextSize);
          },
        }}
      />

      <Modal title="Create Snapshot from Sandbox" open={createOpen} onCancel={() => setCreateOpen(false)} footer={null}>
        <Form layout="vertical" onFinish={onCreate}>
          <Form.Item name="sandboxId" label="Sandbox ID" rules={[{ required: true }]}>
            <Input />
          </Form.Item>
          <Form.Item name="name" label="Name (optional)">
            <Input />
          </Form.Item>
          <Button type="primary" htmlType="submit" block>
            Create
          </Button>
        </Form>
      </Modal>
    </div>
  );
}
