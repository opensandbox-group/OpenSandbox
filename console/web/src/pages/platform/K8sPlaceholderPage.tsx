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

import { Alert, Typography } from 'antd';

type Props = {
  title: string;
};

export function K8sPlaceholderPage({ title }: Props) {
  return (
    <div>
      <Typography.Title level={4}>{title}</Typography.Title>
      <Alert
        type="warning"
        showIcon
        message="Phase 2 — 需要 K8s RBAC"
        description={
          <ul>
            <li>BFF 需配置只读 ServiceAccount（list/watch Pods、Events 等）。</li>
            <li>当前版本未实现 K8s API 代理；本页为结构化占位，避免误导为已联通集群。</li>
            <li>上线后在此展示按 namespace 过滤的工作负载或事件表格。</li>
          </ul>
        }
      />
    </div>
  );
}
