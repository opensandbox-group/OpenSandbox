#!/usr/bin/env bash
# Build 5 cumulative console PR branches from feat/developer-console (no server changes).
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"
SOURCE="${SOURCE_BRANCH:-feat/developer-console}"
MAIN="${MAIN_BRANCH:-main}"

if ! git rev-parse --verify "$SOURCE" >/dev/null 2>&1; then
  echo "Missing source branch: $SOURCE" >&2
  exit 1
fi

git fetch origin "$MAIN" 2>/dev/null || true

pr1_layout() {
  cat > console/web/src/layout/AppLayout.tsx <<'EOF'
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

import { DashboardOutlined, PlusOutlined, UnorderedListOutlined } from '@ant-design/icons';
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
    ...(isAdmin
      ? [{ key: '/', icon: <UnorderedListOutlined />, label: '沙箱（PR3+）', disabled: true }]
      : [
          { key: '/sandboxes', icon: <UnorderedListOutlined />, label: '沙箱' },
          { key: '/sandboxes/new', icon: <PlusOutlined />, label: '创建沙箱' },
        ]),
  ];

  const selectedKey =
    location.pathname === '/sandboxes/new'
      ? '/sandboxes/new'
      : location.pathname.startsWith('/sandboxes')
        ? '/sandboxes'
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
          items={items}
          onClick={({ key }) => navigate(key)}
        />
      </Sider>
      <Layout>
        <Header style={{ background: '#fff', display: 'flex', justifyContent: 'flex-end', paddingInline: 24 }}>
          <Typography.Link onClick={() => void logout()}>退出</Typography.Link>
        </Header>
        <Content style={{ margin: 24 }}>
          <Outlet />
        </Content>
      </Layout>
    </Layout>
  );
}
EOF
}

pr1_app() {
  cat > console/web/src/App.tsx <<'EOF'
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

import { ConfigProvider } from 'antd';
import zhCN from 'antd/locale/zh_CN';
import { BrowserRouter, Navigate, Route, Routes } from 'react-router-dom';

import { AuthProvider, RequireAuth } from './auth/AuthContext';
import { AppLayout } from './layout/AppLayout';
import { LoginPage } from './pages/LoginPage';
import { OverviewPage } from './pages/OverviewPage';
import { SandboxCreatePage } from './pages/SandboxCreatePage';
import { SandboxDetailPage } from './pages/SandboxDetailPage';
import { SandboxListPage } from './pages/SandboxListPage';

export default function App() {
  return (
    <ConfigProvider locale={zhCN}>
      <BrowserRouter>
        <AuthProvider>
          <Routes>
            <Route path="/login" element={<LoginPage />} />
            <Route
              element={
                <RequireAuth>
                  <AppLayout />
                </RequireAuth>
              }
            >
              <Route index element={<OverviewPage />} />
              <Route path="sandboxes" element={<SandboxListPage />} />
              <Route path="sandboxes/new" element={<SandboxCreatePage />} />
              <Route path="sandboxes/:id" element={<SandboxDetailPage />} />
            </Route>
            <Route path="*" element={<Navigate to="/" replace />} />
          </Routes>
        </AuthProvider>
      </BrowserRouter>
    </ConfigProvider>
  );
}
EOF
}

pr1_main_py() {
  cat > console/bff/app/main.py <<'EOF'
# Copyright 2026 The OpenSandbox Authors
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

from __future__ import annotations

from fastapi import FastAPI
from fastapi.middleware.cors import CORSMiddleware

from app.config import get_settings
from app.lifecycle import LifecycleClient
from app.routes import auth, sandboxes

app = FastAPI(title="OpenSandbox Console BFF", version="0.1.0")


@app.on_event("startup")
def validate_config() -> None:
    settings = get_settings()
    if not settings.tenants_toml_path:
        raise RuntimeError("TENANTS_TOML_PATH is required")
    if not settings.bff_session_secret or not settings.bff_admin_token:
        raise RuntimeError("BFF_SESSION_SECRET and BFF_ADMIN_TOKEN are required")


@app.get("/health")
async def health() -> dict[str, str]:
    return {"status": "ok"}


@app.get("/api/version")
async def version() -> dict:
    client = LifecycleClient(get_settings())
    return await client.get_version()


settings = get_settings()
app.add_middleware(
    CORSMiddleware,
    allow_origins=settings.cors_origins_list,
    allow_credentials=True,
    allow_methods=["*"],
    allow_headers=["*"],
)

api = FastAPI()
api.include_router(auth.router)
api.include_router(sandboxes.router)
app.mount("/api", api)
EOF
}

pr1_overview() {
  cat > console/web/src/pages/OverviewPage.tsx <<'EOF'
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

import { Alert, Card, Col, Row, Statistic, Typography } from 'antd';
import { useEffect, useState } from 'react';

import { sandboxApi } from '../api/client';
import { useAuth } from '../auth/AuthContext';

export function OverviewPage() {
  const { user } = useAuth();
  const [total, setTotal] = useState(0);
  const [running, setRunning] = useState(0);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (user?.role === 'admin') return;
    void (async () => {
      setError(null);
      try {
        const list = await sandboxApi.list({ pageSize: 500 });
        const items = list.items ?? [];
        setTotal(items.length);
        setRunning(items.filter((s) => s.status?.state === 'Running').length);
      } catch (e) {
        setError(e instanceof Error ? e.message : 'Failed to load overview');
      }
    })();
  }, [user?.role]);

  if (user?.role === 'admin') {
    return (
      <div>
        <Typography.Title level={4}>概览</Typography.Title>
        <Alert type="info" message="Admin 聚合视图将在 PR3 提供。" />
      </div>
    );
  }

  return (
    <div>
      <Typography.Title level={4}>概览</Typography.Title>
      {error && <Alert type="error" message={error} style={{ marginBottom: 16 }} />}
      <Row gutter={[16, 16]}>
        <Col xs={24} sm={12}>
          <Card>
            <Statistic title="沙箱总数" value={total} />
          </Card>
        </Col>
        <Col xs={24} sm={12}>
          <Card>
            <Statistic title="Running" value={running} />
          </Card>
        </Col>
      </Row>
    </div>
  );
}
EOF
}

pr1_detail_strip() {
  cat > console/web/src/pages/SandboxDetailPage.tsx <<'EOF'
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

import { Alert, Button, Card, Descriptions, InputNumber, Modal, Space, Typography, message } from 'antd';
import { useCallback, useEffect, useState } from 'react';
import { useNavigate, useParams } from 'react-router-dom';

import { sandboxApi } from '../api/client';
import type { Sandbox } from '../api/types';
import { sandboxDisplayName } from '../utils/format';

export function SandboxDetailPage() {
  const { id } = useParams<{ id: string }>();
  const navigate = useNavigate();
  const [sandbox, setSandbox] = useState<Sandbox | null>(null);
  const [loading, setLoading] = useState(true);
  const [renewOpen, setRenewOpen] = useState(false);
  const [renewHours, setRenewHours] = useState(1);
  const [endpointPort, setEndpointPort] = useState<number>(8080);
  const [endpointInfo, setEndpointInfo] = useState<Record<string, unknown> | null>(null);
  const [actionLoading, setActionLoading] = useState(false);

  const load = useCallback(async () => {
    if (!id) return;
    setLoading(true);
    try {
      setSandbox(await sandboxApi.get(id));
    } catch (e) {
      message.error(e instanceof Error ? e.message : '加载失败');
    } finally {
      setLoading(false);
    }
  }, [id]);

  useEffect(() => {
    void load();
  }, [load]);

  if (!id) return null;
  if (loading && !sandbox) return <Typography.Text>加载中…</Typography.Text>;
  if (!sandbox) return <Alert type="error" message="未找到沙箱" />;

  const state = sandbox.status?.state ?? 'Unknown';

  return (
    <div>
      <Typography.Title level={4}>{sandboxDisplayName(sandbox)}</Typography.Title>
      <Card>
        <Descriptions column={1} bordered size="small">
          <Descriptions.Item label="ID">{sandbox.id}</Descriptions.Item>
          <Descriptions.Item label="状态">{state}</Descriptions.Item>
          <Descriptions.Item label="镜像">{sandbox.image?.uri ?? '—'}</Descriptions.Item>
        </Descriptions>
        <Space style={{ marginTop: 16 }}>
          <Button danger loading={actionLoading} onClick={() => void sandboxApi.remove(id).then(() => navigate('/sandboxes'))}>
            删除
          </Button>
          <Button onClick={() => setRenewOpen(true)}>续期</Button>
          <Button
            onClick={() =>
              void sandboxApi.endpoint(id, endpointPort).then(setEndpointInfo)
            }
          >
            获取 Endpoint
          </Button>
          <InputNumber min={1} max={65535} value={endpointPort} onChange={(v) => setEndpointPort(Number(v) || 8080)} />
        </Space>
        {endpointInfo && (
          <pre style={{ marginTop: 16 }}>{JSON.stringify(endpointInfo, null, 2)}</pre>
        )}
      </Card>
      <Modal open={renewOpen} title="续期" onCancel={() => setRenewOpen(false)} onOk={async () => {
        setActionLoading(true);
        try {
          const expiresAt = new Date(Date.now() + renewHours * 3600_000).toISOString();
          await sandboxApi.renewExpiration(id, expiresAt);
          setRenewOpen(false);
          await load();
        } finally {
          setActionLoading(false);
        }
      }}>
        <InputNumber min={1} value={renewHours} onChange={(v) => setRenewHours(Number(v) || 1)} addonAfter="小时" />
      </Modal>
    </div>
  );
}
EOF
}

remove_pr2_plus() {
  git rm -rf --ignore-unmatch \
    console/bff/app/routes/admin.py \
    console/bff/app/routes/admin_proxy.py \
    console/bff/app/routes/pools.py \
    console/bff/app/routes/snapshots.py \
    console/bff/app/k8s_client.py \
    console/bff/app/k8s_platform.py \
    console/bff/app/k8s_probe.py \
    console/bff/app/k8s_resources.py \
    console/bff/app/nodeagent_archive.py \
    console/web/src/pages/AdminSandboxesPage.tsx \
    console/web/src/pages/DiagnosticsPage.tsx \
    console/web/src/pages/SnapshotsPage.tsx \
    console/web/src/pages/PoolsPage.tsx \
    console/web/src/pages/platform \
    console/web/src/components/AdminTenantSelect.tsx \
    console/web/src/components/SandboxArchiveLogPanel.tsx \
    console/web/src/components/SandboxColoredLogViewer.css \
    console/web/src/components/SandboxColoredLogViewer.tsx \
    console/web/src/components/SandboxLogPanel.tsx \
    console/web/src/components/DismissibleAlert.tsx \
    console/web/src/utils/enrichLogText.ts \
    console/web/src/utils/semanticTheme.ts \
    console/k8s/console-bff-rbac.example.yaml \
    console/docs/04-deployment-checklist.md \
    console/docs/05-feature-matrix.md \
    console/docs/06-build-and-deploy.md \
    console/docs/07-sandbox-history.md \
    2>/dev/null || true
  rm -rf console/web/src/pages/platform 2>/dev/null || true
}

checkout_full() {
  git checkout "$SOURCE" -- console/ .github/workflows/console-test.yml
}

echo "== PR1 branch =="
git checkout -B feat/console-pr1-p0 "origin/$MAIN" 2>/dev/null || git checkout -B feat/console-pr1-p0 "$MAIN"
checkout_full
remove_pr2_plus
git checkout "$SOURCE" -- console/web/src/utils/semanticTheme.ts
pr1_main_py
pr1_app
pr1_layout
pr1_overview
pr1_detail_strip
git add -A console/ .github/workflows/console-test.yml
git commit -m "$(cat <<'EOF'
feat(console): add BFF and web for tenant sandbox P0 (PR 1/5)

Introduce console/ with FastAPI BFF session auth, Lifecycle sandbox proxy,
runtimeSummary, and tenant UI for list/create/detail/renew/delete/endpoint.
Includes CI workflow and core console docs (architecture + BFF config).

EOF
)"

echo "== PR2 branch =="
git checkout -B feat/console-pr2-p1 feat/console-pr1-p0
git checkout "$SOURCE" -- \
  console/bff/app/routes/snapshots.py \
  console/bff/app/main.py \
  console/web/src/App.tsx \
  console/web/src/layout/AppLayout.tsx \
  console/web/src/pages/SnapshotsPage.tsx \
  console/web/src/pages/DiagnosticsPage.tsx \
  console/web/src/pages/SandboxDetailPage.tsx \
  console/web/src/components/SandboxLogPanel.tsx \
  console/web/src/components/SandboxColoredLogViewer.tsx \
  console/web/src/components/SandboxColoredLogViewer.css \
  console/web/src/utils/enrichLogText.ts
# PR2 main: auth sandboxes snapshots only
python3 <<'PY'
from pathlib import Path
p = Path("console/bff/app/main.py")
text = p.read_text()
text = text.replace("from app.routes import admin, auth, pools, sandboxes, snapshots", "from app.routes import auth, sandboxes, snapshots")
text = text.replace("api.include_router(pools.router)\n", "")
text = text.replace("api.include_router(admin.router)\n", "")
p.write_text(text)
PY
git add -A console/
git commit -m "$(cat <<'EOF'
feat(console): snapshots and diagnostics UI (PR 2/5)

Add snapshot list/delete routes, diagnostics page, and sandbox detail log tab
with colored log viewer.

EOF
)"

echo "== PR3 branch =="
git checkout -B feat/console-pr3-admin feat/console-pr2-p1
git checkout "$SOURCE" -- \
  console/bff/app/routes/admin.py \
  console/bff/app/routes/admin_proxy.py \
  console/bff/app/routes/pools.py \
  console/bff/app/main.py \
  console/web/src/App.tsx \
  console/web/src/layout/AppLayout.tsx \
  console/web/src/pages/OverviewPage.tsx \
  console/web/src/pages/AdminSandboxesPage.tsx \
  console/web/src/pages/PoolsPage.tsx \
  console/web/src/pages/SandboxDetailPage.tsx \
  console/web/src/components/AdminTenantSelect.tsx \
  console/web/src/components/SemanticTags.tsx \
  console/web/src/components/SemanticTags.css \
  console/web/src/api/client.ts \
  console/web/src/pages/SandboxListPage.tsx
git add -A console/
git commit -m "$(cat <<'EOF'
feat(console): admin aggregation, pools, and tenant picker (PR 3/5)

Cross-tenant sandbox list, admin proxy ops on detail, runtime stats on overview,
Pool management UI, and semantic status tags on lists.

EOF
)"

echo "== PR4 branch =="
git checkout -B feat/console-pr4-platform feat/console-pr3-admin
git checkout "$SOURCE" -- \
  console/bff/app/k8s_client.py \
  console/bff/app/k8s_platform.py \
  console/bff/app/k8s_probe.py \
  console/bff/app/k8s_resources.py \
  console/bff/app/nodeagent_archive.py \
  console/bff/app/routes/admin.py \
  console/bff/app/routes/sandboxes.py \
  console/web/src/App.tsx \
  console/web/src/layout/AppLayout.tsx \
  console/web/src/pages/SandboxDetailPage.tsx \
  console/web/src/components/SandboxArchiveLogPanel.tsx \
  console/web/src/pages/platform \
  console/k8s/console-bff-rbac.example.yaml \
  console/docs/02-bff-config-and-secrets.md
git add -A console/
git commit -m "$(cat <<'EOF'
feat(console): platform health, K8s views, and archive logs (PR 4/5)

Optional in-cluster probes, admin K8s workloads/events pages, component health,
OSS archive log panel, and BFF RBAC example manifest.

EOF
)"

echo "== PR5 branch =="
git checkout -B feat/console-pr5-docs-ci feat/console-pr4-platform
git checkout "$SOURCE" -- \
  console/ \
  .github/workflows/console-test.yml \
  AGENTS.md \
  docs/.vitepress/config.mts \
  docs/guides/developer-console.md
git add -A console/ .github/workflows/console-test.yml AGENTS.md docs/
git commit -m "$(cat <<'EOF'
feat(console): docs site guide, AGENTS routing, and deployment docs (PR 5/5)

Add VitePress developer-console guide, AGENTS.md console routing, full
console/docs deployment matrix and deferred history note. Aligns with MVP scope.

EOF
)" || echo "PR5: nothing to commit or already matches"

echo "Done. Branches: feat/console-pr1-p0 .. feat/console-pr5-docs-ci"
