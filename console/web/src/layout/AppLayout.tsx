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
  DatabaseOutlined,
  FileSearchOutlined,
  HistoryOutlined,
  HomeOutlined,
  LogoutOutlined,
  PieChartOutlined,
  PictureOutlined,
  SettingOutlined,
  UnorderedListOutlined,
} from '@ant-design/icons';
import { Breadcrumb, Layout, Menu, Typography } from 'antd';
import type { MenuProps } from 'antd';
import { useMemo, useState } from 'react';
import { Outlet, useLocation, useNavigate, useParams } from 'react-router-dom';

import { useAuth } from '../auth/AuthContext';
import { buildRouteBreadcrumbs } from './routeBreadcrumbs';

import './AppLayout.css';

const { Header, Sider, Content } = Layout;

const PLATFORM_SUBMENU_KEY = 'platform';

function SiderLogo() {
  return (
    <div className="console-sider-brand-logo" aria-hidden>
      <svg width="36" height="36" viewBox="0 0 36 36" fill="none">
        <path
          d="M18 3L31 10.5V25.5L18 33L5 25.5V10.5L18 3Z"
          stroke="#4096ff"
          strokeWidth="1.5"
          fill="rgba(22, 119, 255, 0.15)"
        />
        <path
          d="M18 11L24 14.5V21.5L18 25L12 21.5V14.5L18 11Z"
          stroke="#69b1ff"
          strokeWidth="1.2"
          fill="rgba(105, 177, 255, 0.25)"
        />
      </svg>
    </div>
  );
}

export function AppLayout() {
  const { user, logout } = useAuth();
  const navigate = useNavigate();
  const location = useLocation();

  const isAdmin = user?.role === 'admin';
  const sandboxListKey = isAdmin ? '/admin/sandboxes' : '/sandboxes';
  const sandboxListLabel = isAdmin ? '沙箱（全局）' : '沙箱';

  const items: MenuProps['items'] = useMemo(
    () => [
      {
        key: '/',
        icon: <HomeOutlined />,
        label: '概览',
      },
      {
        key: sandboxListKey,
        icon: <UnorderedListOutlined />,
        label: sandboxListLabel,
      },
      {
        key: '/history/sandboxes',
        icon: <HistoryOutlined />,
        label: '申请历史',
      },
      {
        key: '/history/images',
        icon: <PictureOutlined />,
        label: '镜像统计',
      },
      {
        key: '/history/usage',
        icon: <PieChartOutlined />,
        label: '用量分摊',
      },
      {
        key: '/snapshots',
        icon: <CameraOutlined />,
        label: '快照',
      },
      ...(isAdmin
        ? [
            {
              key: '/pools',
              icon: <DatabaseOutlined />,
              label: 'Pool',
            },
          ]
        : []),
      {
        key: '/diagnostics',
        icon: <FileSearchOutlined />,
        label: '诊断',
      },
      {
        key: PLATFORM_SUBMENU_KEY,
        icon: <SettingOutlined />,
        label: '平台',
        children: [
          { key: '/platform/health', label: '组件健康' },
          { key: '/platform/version', label: '版本' },
          { key: '/platform/k8s/workloads', label: 'K8s 工作负载' },
          { key: '/platform/k8s/events', label: 'K8s 事件' },
        ],
      },
    ],
    [isAdmin, sandboxListKey, sandboxListLabel],
  );

  const selectedKey = useMemo(() => {
    const path = location.pathname;
    if (path === '/' || path === '') return '/';
    if (path === '/history/sandboxes') return '/history/sandboxes';
    if (path === '/sandboxes/new') return sandboxListKey;
    if (path.startsWith('/admin/sandboxes') || (isAdmin && path.startsWith('/sandboxes/'))) {
      return sandboxListKey;
    }
    if (path === '/sandboxes' || path.startsWith('/sandboxes/')) return sandboxListKey;
    return path;
  }, [location.pathname, isAdmin, sandboxListKey]);

  /** 与参考设计一致：「平台」分组默认展开 */
  const [openKeys, setOpenKeys] = useState<string[]>([PLATFORM_SUBMENU_KEY]);

  const brandSubtitle = isAdmin ? 'Admin' : user?.tenant ?? 'Tenant';
  const params = useParams();
  const breadcrumbItems = useMemo(
    () => buildRouteBreadcrumbs(location.pathname, params, isAdmin),
    [location.pathname, params, isAdmin],
  );

  return (
    <Layout style={{ minHeight: '100vh' }}>
      <Sider breakpoint="lg" collapsedWidth={0} width={228}>
        <div className="console-sider-brand">
          <div className="console-sider-brand-row">
            <SiderLogo />
            <div className="console-sider-brand-text">
              <div className="console-sider-brand-title">OpenSandbox</div>
              <div className="console-sider-brand-sub">{brandSubtitle}</div>
            </div>
          </div>
        </div>
        <Menu
          theme="dark"
          mode="inline"
          className="console-sider-menu"
          selectedKeys={[selectedKey]}
          openKeys={openKeys}
          onOpenChange={setOpenKeys}
          items={items}
          onClick={({ key }) => {
            if (key === PLATFORM_SUBMENU_KEY) return;
            navigate(key);
          }}
        />
      </Sider>
      <Layout>
        <Header className="console-app-header">
          <Breadcrumb className="console-app-header-breadcrumb" items={breadcrumbItems} />
          <div className="console-app-header-actions">
            <Typography.Text type="secondary">
              <AppstoreOutlined /> {user?.namespace ?? 'platform'}
            </Typography.Text>
            <Typography.Link onClick={() => void logout()}>
              <LogoutOutlined /> 退出
            </Typography.Link>
          </div>
        </Header>
        <Content style={{ margin: 24 }}>
          <Outlet />
        </Content>
      </Layout>
    </Layout>
  );
}
