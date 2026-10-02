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

import { SANDBOX_STATES } from './format';

/** Console 内统一的语义色：日志 ANSI、Ant Design Tag 等共用同一套映射。 */

export const LOG_ANSI = {
  reset: '\x1b[0m',
  dim: '\x1b[90m',
  red: '\x1b[31m',
  green: '\x1b[32m',
  yellow: '\x1b[33m',
  blue: '\x1b[34m',
  magenta: '\x1b[35m',
  cyan: '\x1b[36m',
} as const;

const TENANT_TAG_COLORS = [
  'blue',
  'geekblue',
  'purple',
  'cyan',
  'green',
  'orange',
  'gold',
  'magenta',
] as const;

export type TenantTagColor = (typeof TENANT_TAG_COLORS)[number];

/** 租户名稳定映射到 Tag 色，全站表格一致。 */
export function tenantTagColor(tenant: string): TenantTagColor {
  let h = 0;
  for (let i = 0; i < tenant.length; i++) h = (h * 31 + tenant.charCodeAt(i)) >>> 0;
  return TENANT_TAG_COLORS[h % TENANT_TAG_COLORS.length];
}

/** 与 Lifecycle 枚举对齐（大小写不敏感）。 */
export function normalizeSandboxState(state: string): string {
  const trimmed = state.trim();
  const canonical = SANDBOX_STATES.find((s) => s.toLowerCase() === trimmed.toLowerCase());
  return canonical ?? trimmed;
}

/** 沙箱 lifecycle 状态 → Ant Design Tag color。仅 Failed 为 error；Terminated 为正常结束。 */
export function sandboxStateTagColor(state?: string | null): string | undefined {
  if (!state?.trim()) return undefined;
  switch (normalizeSandboxState(state)) {
    case 'Running':
      return 'success';
    case 'Failed':
      return 'error';
    case 'Terminated':
      return 'green';
    case 'Paused':
      return 'cyan';
    case 'Pending':
    case 'Pausing':
    case 'Resuming':
      return 'processing';
    case 'Stopping':
      return 'orange';
    default:
      return 'blue';
  }
}

/** 日志 level（含 zap 的 warn / Python 单字母 I）→ ANSI 前景色转义。 */
export function logLevelAnsi(level: string): string {
  const u = level.toUpperCase();
  if (u === 'ERROR' || u === 'ERR' || u === 'FATAL' || u === 'PANIC' || u === 'CRITICAL' || u === 'E') {
    return LOG_ANSI.red;
  }
  if (u === 'WARN' || u === 'WARNING' || u === 'W') return LOG_ANSI.yellow;
  if (u === 'DEBUG' || u === 'TRACE' || u === 'D') return LOG_ANSI.blue;
  if (u === 'INFO' || u === 'I') return LOG_ANSI.green;
  return LOG_ANSI.reset;
}
