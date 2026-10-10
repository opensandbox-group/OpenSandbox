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

import { Card, Spin, Typography } from 'antd';
import { useEffect, useState } from 'react';

import { platformApi } from '../../api/client';

export function VersionPage() {
  const [version, setVersion] = useState<Record<string, unknown> | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    void (async () => {
      try {
        setVersion(await platformApi.version());
      } catch (e) {
        setError(e instanceof Error ? e.message : '加载失败');
      }
    })();
  }, []);

  if (!version && !error) return <Spin />;

  return (
    <div>
      <Typography.Title level={4}>版本</Typography.Title>
      {error && <Typography.Text type="danger">{error}</Typography.Text>}
      {version && (
        <Card>
          <pre>{JSON.stringify(version, null, 2)}</pre>
        </Card>
      )}
    </div>
  );
}
