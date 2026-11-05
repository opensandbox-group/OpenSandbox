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

import { Button, Typography } from 'antd';
import { Link, useNavigate } from 'react-router-dom';

import { sandboxShellPath } from '../utils/shellPath';

type Props = {
  sandboxId: string;
  state?: string;
  tenant?: string;
  variant?: 'link' | 'button';
};

export function ShellLink({ sandboxId, state, tenant, variant = 'link' }: Props) {
  const navigate = useNavigate();
  const running = state === 'Running' && Boolean(sandboxId);

  if (variant === 'button') {
    return (
      <Button
        disabled={!running}
        onClick={() => navigate(sandboxShellPath(sandboxId, tenant))}
      >
        Shell
      </Button>
    );
  }

  if (!running) {
    return <Typography.Text type="secondary">Shell</Typography.Text>;
  }
  return <Link to={sandboxShellPath(sandboxId, tenant)}>Shell</Link>;
}
