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

import { Tag } from 'antd';

import { sandboxStateTagColor, tenantTagColor } from '../utils/semanticTheme';

import './SemanticTags.css';

type TenantTagProps = {
  tenant?: string | null;
  className?: string;
};

export function TenantTag({ tenant, className }: TenantTagProps) {
  const t = tenant?.trim();
  if (!t) return <>—</>;
  return (
    <Tag color={tenantTagColor(t)} className={['console-semantic-tag', className].filter(Boolean).join(' ')}>
      {t}
    </Tag>
  );
}

type SandboxStateTagProps = {
  state?: string | null;
};

export function SandboxStateTag({ state }: SandboxStateTagProps) {
  const s = state?.trim();
  if (!s) return <>—</>;
  return (
    <Tag color={sandboxStateTagColor(s)} className="console-semantic-tag">
      {s}
    </Tag>
  );
}

/** 表格内 Tag 列：防止窄列溢出到相邻单元格。 */
export const consoleTagTableCellProps = { className: 'console-tag-table-cell' as const };
