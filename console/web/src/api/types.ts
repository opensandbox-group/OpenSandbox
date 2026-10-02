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

export type UserRole = 'tenant' | 'admin';

export interface SessionUser {
  role: UserRole;
  tenant?: string;
  namespace?: string;
}

export interface RuntimeSummary {
  wallClockSeconds?: number;
  remainingSeconds?: number;
  asOf?: string;
  basis?: string;
}

export interface SandboxStatus {
  state?: string;
  message?: string;
  lastTransitionAt?: string;
}

export interface Sandbox {
  id: string;
  createdAt?: string;
  expiresAt?: string;
  image?: { uri?: string };
  entrypoint?: string[];
  metadata?: Record<string, string>;
  status?: SandboxStatus;
  runtimeSummary?: RuntimeSummary;
  tenant?: string;
  namespace?: string;
}

export interface PaginatedSandboxes {
  items: Sandbox[];
  pagination?: {
    page?: number;
    pageSize?: number;
    totalItems?: number;
    tenantErrors?: { tenant: string; code: string; message: string }[];
  };
}

export interface RuntimeStats {
  runningCount: number;
  totalWallClockSeconds: number;
  avgWallClockSeconds: number;
  maxWallClockSeconds: number;
  expiringWithin30mCount: number;
  asOf: string;
}

export interface ResourceLimitsMap {
  cpu?: string;
  memory?: string;
  disk?: string;
  [key: string]: string | undefined;
}

export interface SandboxHistoryItem {
  sandboxId: string;
  tenant?: string;
  namespace?: string;
  state?: string;
  imageUri?: string;
  createdAt?: string;
  expiresAt?: string;
  endedAt?: string;
  deletedAt?: string;
  wallClockSeconds?: number;
  snapshotCount?: number;
  source?: string;
  firstRecordedAt?: string;
  lastSeenAt?: string;
  /** 创建时的 Kubernetes resourceLimits（来自历史入库） */
  resourceLimits?: ResourceLimitsMap;
  resourceRequests?: ResourceLimitsMap;
  cpuCores?: number;
  memoryGi?: number;
  createTimeoutSeconds?: number;
}

export interface SandboxHistoryStats {
  enabled?: boolean;
  totalRecords?: number;
  activeRecords?: number;
  totalWallClockSeconds?: number;
  avgWallClockSeconds?: number;
  asOf?: string;
}

export interface ImageStatsRow {
  imageUri: string;
  sandboxCount: number;
  activeCount: number;
  tenantCount: number;
  sharePercent: number;
  lastUsedAt?: string;
}

export interface ImageStatsResponse {
  enabled?: boolean;
  asOf?: string;
  totalRecords?: number;
  distinctImages?: number;
  items: ImageStatsRow[];
}

export interface SandboxHistoryListResponse {
  items: SandboxHistoryItem[];
  period?: { from?: string; to?: string };
  pagination?: {
    page?: number;
    pageSize?: number;
    totalItems?: number;
    totalPages?: number;
    hasNextPage?: boolean;
  };
}

export interface TenantUsageSharePercent {
  time?: number;
  cpu?: number;
  memory?: number;
  /** Equal-weight mean of time/cpu/memory shares; sums to ~100% across tenants. */
  composite?: number;
}

export interface TenantUsageRow {
  tenant: string;
  sandboxCount: number;
  overlapSeconds: number;
  cpuCoreSeconds: number;
  memoryGiSeconds: number;
  sharePercent?: TenantUsageSharePercent;
}

export interface TenantUsageResponse {
  enabled?: boolean;
  basis?: string;
  coverageNote?: string;
  shareFormula?: Record<string, string>;
  period?: { from?: string; to?: string };
  totals?: {
    sandboxCount?: number;
    overlapSeconds?: number;
    cpuCoreSeconds?: number;
    memoryGiSeconds?: number;
  };
  tenants?: TenantUsageRow[];
}

export interface Snapshot {
  id: string;
  name?: string;
  sourceSandboxId?: string;
  status?: { state?: string; message?: string };
  createdAt?: string;
}

export interface SnapshotListResponse {
  items: Snapshot[];
  pagination?: { totalItems?: number };
}

export interface PoolItem {
  name: string;
  namespace?: string;
  status?: { state?: string; message?: string };
  spec?: Record<string, unknown>;
}

export interface K8sWorkloadRow {
  tenant?: string;
  namespace?: string;
  name?: string;
  sandboxId?: string;
  phase?: string;
  nodeName?: string;
  podIP?: string;
  startTime?: string;
  ready?: boolean | string;
}

export interface K8sEventRow {
  tenant?: string;
  namespace?: string;
  type?: string;
  reason?: string;
  message?: string;
  count?: number;
  lastTimestamp?: string;
  involvedObject?: { kind?: string; name?: string };
}

export interface K8sListResponse<T> {
  items: T[];
  totalItems?: number;
  disabled?: boolean;
  message?: string;
}

export interface PoolListResponse {
  items: PoolItem[];
}

export interface DiagnosticContentResponse {
  sandboxId?: string;
  kind?: string;
  scope?: string;
  delivery?: string;
  content?: string;
  contentUrl?: string;
  contentLength?: number;
  truncated?: boolean;
  warnings?: string[];
  archive?: {
    objectKey?: string;
    prefix?: string;
    markerStatus?: string | null;
  };
}

export interface PlatformSummary {
  asOf?: string;
  server?: {
    health?: { status?: string };
    healthError?: string | null;
    version?: Record<string, unknown>;
    versionError?: string | null;
  };
  components?: Record<string, {
    status?: string;
    note?: string;
    readyPods?: number;
    totalPods?: number;
    sampleReasons?: string[];
  }>;
}

export interface ApiErrorBody {
  code?: string;
  message?: string;
}
