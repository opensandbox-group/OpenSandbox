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

import type { BreadcrumbProps } from 'antd';
import { Link } from 'react-router-dom';

function crumbLink(to: string, label: string) {
  return <Link to={to}>{label}</Link>;
}

function shortId(id: string, max = 12): string {
  if (id.length <= max) return id;
  return `${id.slice(0, max)}…`;
}

const PLATFORM_LABELS: Record<string, string> = {
  '/platform/health': '组件健康',
  '/platform/version': '版本',
  '/platform/k8s/workloads': 'K8s 工作负载',
  '/platform/k8s/events': 'K8s 事件',
};

/** 顶栏面包屑：与路由一一对应，末级为当前页（不可点）。 */
export function buildRouteBreadcrumbs(
  pathname: string,
  params: { id?: string },
  isAdmin: boolean,
): BreadcrumbProps['items'] {
  const home = { title: crumbLink('/', '概览') };
  const runningListPath = isAdmin ? '/admin/sandboxes' : '/sandboxes';
  const runningLabel = isAdmin ? '沙箱（全局）' : '沙箱';

  if (pathname === '/' || pathname === '') {
    return [{ title: '概览' }];
  }

  if (pathname === runningListPath) {
    return [home, { title: runningLabel }];
  }

  if (pathname === '/sandboxes/new') {
    return [home, { title: crumbLink('/sandboxes', runningLabel) }, { title: '创建沙箱' }];
  }

  if (pathname.startsWith('/sandboxes/') && params.id) {
    return [
      home,
      { title: crumbLink(runningListPath, runningLabel) },
      { title: shortId(params.id) },
    ];
  }

  if (pathname === '/history/sandboxes') {
    return [home, { title: '申请历史' }];
  }

  if (pathname === '/history/images') {
    return [home, { title: '镜像统计' }];
  }

  if (pathname === '/history/usage') {
    return [home, { title: '用量分摊' }];
  }

  if (pathname === '/snapshots') {
    return [home, { title: '快照' }];
  }

  if (pathname === '/pools') {
    return [home, { title: 'Pool' }];
  }

  if (pathname === '/diagnostics') {
    return [home, { title: '诊断' }];
  }

  const platformLabel = PLATFORM_LABELS[pathname];
  if (platformLabel) {
    return [home, { title: '平台' }, { title: platformLabel }];
  }

  return [home, { title: pathname }];
}
