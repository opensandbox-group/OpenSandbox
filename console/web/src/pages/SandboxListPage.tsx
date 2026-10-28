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

import { PlusOutlined } from '@ant-design/icons';
import { Button, Select, Space, Table, Typography, message } from 'antd';
import type { ColumnsType } from 'antd/es/table';
import { useCallback, useEffect, useRef, useState } from 'react';
import { Link, useNavigate } from 'react-router-dom';

import { sandboxApi } from '../api/client';
import type { Sandbox } from '../api/types';
import { SandboxStateTag } from '../components/SemanticTags';
import { formatDateTime, formatDuration, SANDBOX_STATES, sandboxDisplayName } from '../utils/format';

export function SandboxListPage() {
  const navigate = useNavigate();
  const [loading, setLoading] = useState(false);
  const [stateFilter, setStateFilter] = useState<string | undefined>();
  const [page, setPage] = useState(1);
  const [pageSize, setPageSize] = useState(20);
  const [total, setTotal] = useState(0);
  const [items, setItems] = useState<Sandbox[]>([]);
  const requestSeq = useRef(0);

  const load = useCallback(async () => {
    const seq = ++requestSeq.current;
    setLoading(true);
    try {
      const data = await sandboxApi.list({
        page,
        pageSize,
        state: stateFilter,
      });
      if (seq !== requestSeq.current) return;
      setItems(data.items ?? []);
      setTotal(data.pagination?.totalItems ?? data.items?.length ?? 0);
    } catch (e) {
      if (seq !== requestSeq.current) return;
      message.error(e instanceof Error ? e.message : '加载失败');
    } finally {
      if (seq === requestSeq.current) setLoading(false);
    }
  }, [page, pageSize, stateFilter]);

  useEffect(() => {
    void load();
  }, [load]);

  const columns: ColumnsType<Sandbox> = [
    {
      title: '名称 / ID',
      render: (_, r) => (
        <Link to={`/sandboxes/${r.id}`}>{sandboxDisplayName(r)}</Link>
      ),
    },
    {
      title: '状态',
      dataIndex: ['status', 'state'],
      render: (s: string | undefined) => <SandboxStateTag state={s} />,
    },
    {
      title: '镜像',
      render: (_, r) => r.image?.uri ?? '—',
      ellipsis: true,
    },
    {
      title: '运行时长',
      render: (_, r) => formatDuration(r.runtimeSummary?.wallClockSeconds),
    },
    {
      title: '剩余',
      render: (_, r) =>
        r.runtimeSummary?.remainingSeconds !== undefined
          ? formatDuration(r.runtimeSummary.remainingSeconds)
          : '—',
    },
    {
      title: '创建时间',
      dataIndex: 'createdAt',
      width: 200,
      render: (v: string | undefined) => formatDateTime(v),
    },
    {
      title: '操作',
      render: (_, r) => (
        <Space>
          <Link to={`/sandboxes/${r.id}?tab=logs`}>日志</Link>
        </Space>
      ),
    },
  ];

  return (
    <div>
      <Space style={{ marginBottom: 16, width: '100%', justifyContent: 'space-between' }}>
        <Typography.Title level={4} style={{ margin: 0 }}>
          沙箱
        </Typography.Title>
        <Space>
          <Select
            allowClear
            placeholder="按状态筛选"
            style={{ width: 180 }}
            value={stateFilter}
            onChange={(value) => {
              setStateFilter(value);
              setPage(1);
            }}
            options={SANDBOX_STATES.map((s) => ({ label: s, value: s }))}
          />
          <Button onClick={() => void load()}>刷新</Button>
          <Button type="primary" icon={<PlusOutlined />} onClick={() => navigate('/sandboxes/new')}>
            创建沙箱
          </Button>
        </Space>
      </Space>
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
    </div>
  );
}
