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
import { LanguageSelect } from '../components/LanguageSelect';
import { useI18n } from '../i18n';

export function LoginPage() {
  const navigate = useNavigate();
  const { refresh } = useAuth();
  const { t } = useI18n();
  const [loading, setLoading] = useState(false);

  const onTenant = async (values: { apiKey: string }) => {
    setLoading(true);
    try {
      await authApi.loginTenant(values.apiKey.trim());
      await refresh();
      message.success(t('login.tenantSuccess'));
      navigate('/', { replace: true });
    } catch (e) {
      message.error(e instanceof Error ? e.message : t('login.failed'));
    } finally {
      setLoading(false);
    }
  };

  const onAdmin = async (values: { adminToken: string }) => {
    setLoading(true);
    try {
      await authApi.loginAdmin(values.adminToken.trim());
      await refresh();
      message.success(t('login.adminSuccess'));
      navigate('/', { replace: true });
    } catch (e) {
      message.error(e instanceof Error ? e.message : t('login.failed'));
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
        position: 'relative',
      }}
    >
      <div style={{ position: 'absolute', top: 24, right: 24 }}>
        <LanguageSelect />
      </div>
      <Card style={{ width: 420, maxWidth: '100%' }}>
        <Typography.Title level={3} style={{ marginTop: 0 }}>
          {t('login.title')}
        </Typography.Title>
        <Alert
          type="info"
          showIcon
          message={t('login.apiKeyNotice')}
          style={{ marginBottom: 16 }}
        />
        <Tabs
          items={[
            {
              key: 'tenant',
              label: t('login.tenantTab'),
              children: (
                <Form layout="vertical" onFinish={onTenant}>
                  <Form.Item
                    name="apiKey"
                    label={t('login.tenantApiKey')}
                    rules={[{ required: true, message: t('login.tenantApiKeyRequired') }]}
                  >
                    <Input.Password prefix={<UserOutlined />} placeholder="OPEN-SANDBOX-API-KEY" />
                  </Form.Item>
                  <Button type="primary" htmlType="submit" block loading={loading}>
                    {t('login.signIn')}
                  </Button>
                </Form>
              ),
            },
            {
              key: 'admin',
              label: t('login.adminTab'),
              children: (
                <Form layout="vertical" onFinish={onAdmin}>
                  <Form.Item
                    name="adminToken"
                    label={t('login.adminToken')}
                    rules={[{ required: true, message: t('login.adminTokenRequired') }]}
                  >
                    <Input.Password prefix={<LockOutlined />} />
                  </Form.Item>
                  <Button type="primary" htmlType="submit" block loading={loading}>
                    {t('login.signIn')}
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
