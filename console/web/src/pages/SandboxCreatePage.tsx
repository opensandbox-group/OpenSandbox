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

import { MinusCircleOutlined, PlusOutlined } from '@ant-design/icons';
import { Button, Form, Input, InputNumber, Space, Typography, message } from 'antd';
import { useState } from 'react';
import { useNavigate } from 'react-router-dom';

import { sandboxApi } from '../api/client';

type EnvRow = { key: string; value: string };
type MetaRow = { key: string; value: string };

export function SandboxCreatePage() {
  const navigate = useNavigate();
  const [submitting, setSubmitting] = useState(false);

  const onFinish = async (values: {
    imageUri: string;
    entrypoint: string;
    cpu: string;
    memory: string;
    timeout?: number;
    env?: EnvRow[];
    metadata?: MetaRow[];
  }) => {
    setSubmitting(true);
    try {
      const entrypoint = values.entrypoint
        .split(/\s+/)
        .map((s) => s.trim())
        .filter(Boolean);
      const env: Record<string, string> = {};
      (values.env ?? []).forEach(({ key, value }) => {
        if (key) env[key] = value;
      });
      const metadata: Record<string, string> = {};
      (values.metadata ?? []).forEach(({ key, value }) => {
        if (key) metadata[key] = value;
      });

      const body: Record<string, unknown> = {
        image: { uri: values.imageUri.trim() },
        entrypoint,
        resourceLimits: { cpu: values.cpu.trim(), memory: values.memory.trim() },
      };
      if (values.timeout) body.timeout = values.timeout;
      if (Object.keys(env).length) body.env = env;
      if (Object.keys(metadata).length) body.metadata = metadata;

      const created = await sandboxApi.create(body);
      message.success('沙箱已创建');
      navigate(`/sandboxes/${created.id}`);
    } catch (e) {
      message.error(e instanceof Error ? e.message : '创建失败');
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <div>
      <Typography.Title level={4}>创建沙箱</Typography.Title>
      <Form
        layout="vertical"
        onFinish={onFinish}
        initialValues={{ entrypoint: 'tail -f /dev/null', timeout: 3600, cpu: '500m', memory: '512Mi' }}
        style={{ maxWidth: 640 }}
      >
        <Form.Item
          name="imageUri"
          label="镜像 URI"
          rules={[{ required: true, message: '请输入镜像' }]}
        >
          <Input placeholder="python:3.11" />
        </Form.Item>
        <Form.Item
          name="entrypoint"
          label="Entrypoint（空格分隔）"
          rules={[{ required: true, message: '请输入 entrypoint' }]}
        >
          <Input placeholder="python /app/main.py" />
        </Form.Item>
        <Form.Item name="cpu" label="CPU" rules={[{ required: true, message: '请输入 CPU' }]}>
          <Input placeholder="500m" />
        </Form.Item>
        <Form.Item name="memory" label="内存" rules={[{ required: true, message: '请输入内存' }]}>
          <Input placeholder="512Mi" />
        </Form.Item>
        <Form.Item name="timeout" label="超时（秒）">
          <InputNumber min={60} style={{ width: '100%' }} />
        </Form.Item>

        <Typography.Text strong>环境变量</Typography.Text>
        <Form.List name="env">
          {(fields, { add, remove }) => (
            <>
              {fields.map(({ key, name, ...rest }) => (
                <Space key={key} align="baseline" style={{ display: 'flex', marginBottom: 8 }}>
                  <Form.Item {...rest} name={[name, 'key']} rules={[{ required: true }]}>
                    <Input placeholder="KEY" />
                  </Form.Item>
                  <Form.Item {...rest} name={[name, 'value']}>
                    <Input placeholder="value" />
                  </Form.Item>
                  <MinusCircleOutlined onClick={() => remove(name)} />
                </Space>
              ))}
              <Form.Item>
                <Button type="dashed" onClick={() => add()} block icon={<PlusOutlined />}>
                  添加环境变量
                </Button>
              </Form.Item>
            </>
          )}
        </Form.List>

        <Typography.Text strong>Metadata</Typography.Text>
        <Form.List name="metadata">
          {(fields, { add, remove }) => (
            <>
              {fields.map(({ key, name, ...rest }) => (
                <Space key={key} align="baseline" style={{ display: 'flex', marginBottom: 8 }}>
                  <Form.Item {...rest} name={[name, 'key']} rules={[{ required: true }]}>
                    <Input placeholder="name" />
                  </Form.Item>
                  <Form.Item {...rest} name={[name, 'value']}>
                    <Input placeholder="value" />
                  </Form.Item>
                  <MinusCircleOutlined onClick={() => remove(name)} />
                </Space>
              ))}
              <Form.Item>
                <Button type="dashed" onClick={() => add()} block icon={<PlusOutlined />}>
                  添加 metadata
                </Button>
              </Form.Item>
            </>
          )}
        </Form.List>

        <Space>
          <Button type="primary" htmlType="submit" loading={submitting}>
            提交
          </Button>
          <Button onClick={() => navigate('/sandboxes')}>取消</Button>
        </Space>
      </Form>
    </div>
  );
}
