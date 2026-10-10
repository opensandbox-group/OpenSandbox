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

export function AppLayout() {
  const { user, logout } = useAuth();
  const navigate = useNavigate();
  const location = useLocation();

  const isAdmin = user?.role === 'admin';

  const items = [
    { key: '/', icon: <DashboardOutlined />, label: '概览' },
    isAdmin
      ? { key: '/admin/sandboxes', icon: <UnorderedListOutlined />, label: '沙箱（全局）' }
      : { key: '/sandboxes', icon: <UnorderedListOutlined />, label: '沙箱' },
    ...(isAdmin
      ? []
      : [{ key: '/sandboxes/new', icon: <PlusOutlined />, label: '创建沙箱' }]),
    { key: '/snapshots', icon: <CameraOutlined />, label: '快照' },
    ...(isAdmin ? [{ key: '/pools', icon: <DatabaseOutlined />, label: 'Pool' }] : []),
    { key: '/diagnostics', icon: <FileSearchOutlined />, label: '诊断' },
    {
      key: 'platform',
      icon: <SettingOutlined />,
      label: '平台',
      children: [
        { key: '/platform/health', label: '组件健康' },
        { key: '/platform/version', label: '版本' },
        { key: '/platform/k8s/workloads', label: 'K8s 工作负载' },
        { key: '/platform/k8s/events', label: 'K8s 事件' },
      ],
    },
  ];

  const selectedKey =
    location.pathname === '/sandboxes/new'
      ? '/sandboxes/new'
      : location.pathname.startsWith('/sandboxes/') && location.pathname !== '/sandboxes/new'
        ? isAdmin
          ? '/admin/sandboxes'
          : '/sandboxes'
        : location.pathname;

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
            <LogoutOutlined /> 退出
          </Typography.Link>
        </Header>
        <Content style={{ margin: 24 }}>
          <Outlet />
        </Content>
      </Layout>
    </Layout>
  );
}
