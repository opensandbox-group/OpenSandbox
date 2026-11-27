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

import {
  AppstoreOutlined,
  CameraOutlined,
  DashboardOutlined,
  DatabaseOutlined,
  FileSearchOutlined,
  LogoutOutlined,
  PlusOutlined,
  SettingOutlined,
  UnorderedListOutlined,
} from '@ant-design/icons';
import { Layout, Menu, Typography } from 'antd';
import { Outlet, useLocation, useNavigate } from 'react-router-dom';

import { useAuth } from '../auth/AuthContext';
import { LanguageSelect } from '../components/LanguageSelect';
import { useI18n } from '../i18n';

const { Header, Sider, Content } = Layout;

function menuKey(pathname: string, isAdmin: boolean): string {
  if (pathname === '/sandboxes/new') return '/sandboxes/new';
  if (pathname.startsWith('/admin/sandboxes')) return '/admin/sandboxes';
  if (pathname.startsWith('/sandboxes')) return isAdmin ? '/admin/sandboxes' : '/sandboxes';
  return pathname;
}

export function AppLayout() {
  const { user, logout } = useAuth();
  const navigate = useNavigate();
  const location = useLocation();
  const { t } = useI18n();

  const isAdmin = user?.role === 'admin';

  const items = [
    { key: '/', icon: <DashboardOutlined />, label: t('common.overview') },
    isAdmin
      ? {
          key: '/admin/sandboxes',
          icon: <UnorderedListOutlined />,
          label: t('nav.sandboxesAllTenants'),
        }
      : { key: '/sandboxes', icon: <UnorderedListOutlined />, label: t('nav.sandboxes') },
    ...(isAdmin
      ? []
      : [
          {
            key: '/sandboxes/new',
            icon: <PlusOutlined />,
            label: t('common.createSandbox'),
          },
        ]),
    { key: '/snapshots', icon: <CameraOutlined />, label: t('nav.snapshots') },
    ...(isAdmin ? [{ key: '/pools', icon: <DatabaseOutlined />, label: t('nav.pool') }] : []),
    { key: '/diagnostics', icon: <FileSearchOutlined />, label: t('nav.diagnostics') },
    {
      key: 'platform',
      icon: <SettingOutlined />,
      label: t('nav.platform'),
      children: [
        { key: '/platform/health', label: t('nav.componentHealth') },
        { key: '/platform/version', label: t('nav.version') },
        ...(isAdmin
          ? [
              { key: '/platform/k8s/workloads', label: t('nav.k8sWorkloads') },
              { key: '/platform/k8s/events', label: t('nav.k8sEvents') },
            ]
          : []),
      ],
    },
  ];

  const selectedKey = menuKey(location.pathname, Boolean(isAdmin));

  return (
    <Layout style={{ minHeight: '100vh' }}>
      <Sider breakpoint="lg" collapsedWidth={0}>
        <div style={{ padding: 16 }}>
          <Typography.Title level={5} style={{ color: '#fff', margin: 0 }}>
            OpenSandbox
          </Typography.Title>
          <Typography.Text style={{ color: 'rgba(255,255,255,0.65)', fontSize: 12 }}>
            {isAdmin ? t('nav.admin') : user?.tenant}
          </Typography.Text>
        </div>
        <Menu
          theme="dark"
          mode="inline"
          selectedKeys={[selectedKey]}
          defaultOpenKeys={['platform']}
          items={items}
          onClick={({ key }) => {
            if (key !== 'platform') navigate(key);
          }}
        />
      </Sider>
      <Layout>
        <Header
          style={{
            background: '#fff',
            display: 'flex',
            alignItems: 'center',
            justifyContent: 'flex-end',
            paddingInline: 24,
            gap: 16,
          }}
        >
          <Typography.Text type="secondary">
            <AppstoreOutlined /> {user?.namespace ?? t('nav.platformFallback')}
          </Typography.Text>
          <LanguageSelect />
          <Typography.Link onClick={() => void logout()}>
            <LogoutOutlined /> {t('nav.signOut')}
          </Typography.Link>
        </Header>
        <Content style={{ margin: 24 }}>
          <Outlet />
        </Content>
      </Layout>
    </Layout>
  );
}
