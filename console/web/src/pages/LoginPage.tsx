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

import { LockOutlined, UserOutlined } from '@ant-design/icons';
import { Alert, Button, Card, Form, Input, Tabs, Typography, message } from 'antd';
import { useState } from 'react';
import { useNavigate } from 'react-router-dom';

import { authApi } from '../api/client';
import { useAuth } from '../auth/AuthContext';

export function LoginPage() {
  const navigate = useNavigate();
  const { refresh } = useAuth();
  const [loading, setLoading] = useState(false);

  const onTenant = async (values: { apiKey: string }) => {
    setLoading(true);
    try {
      await authApi.loginTenant(values.apiKey.trim());
      await refresh();
      message.success('Tenant login successful');
      navigate('/', { replace: true });
    } catch (e) {
      message.error(e instanceof Error ? e.message : 'Login failed');
    } finally {
      setLoading(false);
    }
  };

  const onAdmin = async (values: { adminToken: string }) => {
    setLoading(true);
    try {
      await authApi.loginAdmin(values.adminToken.trim());
      await refresh();
      message.success('Admin login successful');
      navigate('/', { replace: true });
    } catch (e) {
      message.error(e instanceof Error ? e.message : 'Login failed');
    } finally {
      setLoading(false);
    }
  };

  return (
    <div
      style={{
        minHeight: '100vh',
        display: 'flex',
        alignItems: 'center',
        justifyContent: 'center',
        background: '#f0f2f5',
        padding: 24,
      }}
    >
      <Card style={{ width: 420, maxWidth: '100%' }}>
        <Typography.Title level={3} style={{ marginTop: 0 }}>
          OpenSandbox Console
        </Typography.Title>
        <Alert
          type="info"
          showIcon
          message="The API key is sent only to the BFF. The browser does not store the Lifecycle key."
          style={{ marginBottom: 16 }}
        />
        <Tabs
          items={[
            {
              key: 'tenant',
              label: 'Tenant User',
              children: (
                <Form layout="vertical" onFinish={onTenant}>
                  <Form.Item
                    name="apiKey"
                    label="Tenant API Key"
                    rules={[{ required: true, message: 'Enter an API key' }]}
                  >
                    <Input.Password prefix={<UserOutlined />} placeholder="OPEN-SANDBOX-API-KEY" />
                  </Form.Item>
                  <Button type="primary" htmlType="submit" block loading={loading}>
                    Sign In
                  </Button>
                </Form>
              ),
            },
            {
              key: 'admin',
              label: 'Platform Admin',
              children: (
                <Form layout="vertical" onFinish={onAdmin}>
                  <Form.Item
                    name="adminToken"
                    label="BFF Admin Token"
                    rules={[{ required: true, message: 'Enter an admin token' }]}
                  >
                    <Input.Password prefix={<LockOutlined />} />
                  </Form.Item>
                  <Button type="primary" htmlType="submit" block loading={loading}>
                    Sign In
                  </Button>
                </Form>
              ),
            },
          ]}
        />
      </Card>
    </div>
  );
}
