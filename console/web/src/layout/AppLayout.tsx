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

  const isAdmin = user?.role === 'admin';

  const items = [
    { key: '/', icon: <DashboardOutlined />, label: 'Overview' },
    isAdmin
      ? { key: '/admin/sandboxes', icon: <UnorderedListOutlined />, label: 'Sandboxes (All Tenants)' }
      : { key: '/sandboxes', icon: <UnorderedListOutlined />, label: 'Sandboxes' },
    ...(isAdmin
      ? []
      : [{ key: '/sandboxes/new', icon: <PlusOutlined />, label: 'Create Sandbox' }]),
    { key: '/snapshots', icon: <CameraOutlined />, label: 'Snapshots' },
    ...(isAdmin ? [{ key: '/pools', icon: <DatabaseOutlined />, label: 'Pool' }] : []),
    { key: '/diagnostics', icon: <FileSearchOutlined />, label: 'Diagnostics' },
    {
      key: 'platform',
      icon: <SettingOutlined />,
      label: 'Platform',
      children: [
        { key: '/platform/health', label: 'Component Health' },
        { key: '/platform/version', label: 'Version' },
        ...(isAdmin
          ? [
              { key: '/platform/k8s/workloads', label: 'K8s Workloads' },
              { key: '/platform/k8s/events', label: 'K8s Events' },
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
            {isAdmin ? 'Admin' : user?.tenant}
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
            <AppstoreOutlined /> {user?.namespace ?? 'platform'}
          </Typography.Text>
          <Typography.Link onClick={() => void logout()}>
            <LogoutOutlined /> Sign Out
          </Typography.Link>
        </Header>
        <Content style={{ margin: 24 }}>
          <Outlet />
        </Content>
      </Layout>
    </Layout>
  );
}
