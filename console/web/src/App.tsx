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
import { AdminSandboxesPage } from './pages/AdminSandboxesPage';
import { DiagnosticsPage } from './pages/DiagnosticsPage';
import { LoginPage } from './pages/LoginPage';
import { OverviewPage } from './pages/OverviewPage';
import { PoolsPage } from './pages/PoolsPage';
import { SandboxCreatePage } from './pages/SandboxCreatePage';
import { SandboxDetailPage } from './pages/SandboxDetailPage';
import { SandboxListPage } from './pages/SandboxListPage';
import { SnapshotsPage } from './pages/SnapshotsPage';

const consoleTheme = {
  token: {
    borderRadius: 8,
    colorPrimary: '#1677ff',
    fontSize: 14,
  },
  components: {
    Menu: {
      itemHeight: 40,
      itemMarginInline: 8,
      iconSize: 16,
    },
    Card: {
      borderRadiusLG: 10,
      paddingLG: 20,
    },
    Table: {
      headerBg: '#fafafa',
    },
    Breadcrumb: {
      fontSize: 14,
    },
  },
};

export default function App() {
  return (
    <ConfigProvider locale={zhCN} theme={consoleTheme}>
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
              <Route path="admin/sandboxes" element={<AdminSandboxesPage />} />
              <Route path="snapshots" element={<SnapshotsPage />} />
              <Route path="pools" element={<PoolsPage />} />
              <Route path="diagnostics" element={<DiagnosticsPage />} />
            </Route>
            <Route path="*" element={<Navigate to="/" replace />} />
          </Routes>
        </AuthProvider>
      </BrowserRouter>
    </ConfigProvider>
  );
}
