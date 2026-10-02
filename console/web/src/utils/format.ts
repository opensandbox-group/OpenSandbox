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

export function formatDuration(seconds?: number): string {
  if (seconds === undefined || Number.isNaN(seconds)) return '—';
  const s = Math.max(0, Math.floor(seconds));
  const h = Math.floor(s / 3600);
  const m = Math.floor((s % 3600) / 60);
  const sec = s % 60;
  if (h > 0) return `${h}h ${m}m`;
  if (m > 0) return `${m}m ${sec}s`;
  return `${sec}s`;
}

export function sandboxDisplayName(s: { id: string; metadata?: Record<string, string> }): string {
  return s.metadata?.name?.trim() || s.id;
}

export const SANDBOX_STATES = [
  'Pending',
  'Running',
  'Pausing',
  'Paused',
  'Resuming',
  'Stopping',
  'Terminated',
  'Failed',
] as const;

export const DIAGNOSTIC_SCOPES = [
  'container',
  'lifecycle',
  'runtime',
  'network',
  'process',
  'all',
] as const;
