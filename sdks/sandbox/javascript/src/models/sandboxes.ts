// Copyright 2026 Alibaba Group Holding Ltd.
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

/**
 * Domain models for sandbox lifecycle.
 *
 * IMPORTANT:
 * - These are NOT OpenAPI-generated types.
 * - They are intentionally stable and JS-friendly.
 *
 * The internal OpenAPI schemas may change frequently; adapters map responses into these models.
 */

export type SandboxId = string;

export interface ImageAuth extends Record<string, unknown> {
  username?: string;
  password?: string;
  token?: string;
}

export interface ImageSpec {
  uri: string;
  auth?: ImageAuth;
}

export interface PlatformSpec extends Record<string, unknown> {
  /**
   * Target operating system for sandbox provisioning.
   */
  os: "linux" | "windows";
  /**
   * Target CPU architecture for sandbox provisioning.
   */
  arch: "amd64" | "arm64";
}

export type ResourceLimits = Record<string, string>;

export type NetworkRuleAction = "allow" | "deny";

export interface NetworkRule extends Record<string, unknown> {
  /**
   * Whether to allow or deny matching targets.
   */
  action: NetworkRuleAction;
  /**
   * FQDN or wildcard domain (e.g., "example.com", "*.example.com").
   * IP/CIDR is not supported in the egress MVP.
   */
  target: string;
}

export interface NetworkPolicy extends Record<string, unknown> {
  /**
   * Default action when no egress rule matches. Defaults to "deny".
   */
  defaultAction?: NetworkRuleAction;
  /**
   * List of egress rules evaluated in order.
   */
  egress?: NetworkRule[];
}

// ============================================================================
// Volume Models
// ============================================================================

/**
 * Host path bind mount backend.
 *
 * Maps a directory on the host filesystem into the container.
 * Only available when the runtime supports host mounts.
 */
export interface Host extends Record<string, unknown> {
  /**
   * Absolute path on the host filesystem to mount.
   * Must start with '/' (Unix) or a drive letter such as 'C:\' or 'D:/'
   * (Windows), and be under an allowed prefix.
   */
  path: string;
}

/**
 * Platform-managed named volume backend.
 *
 * Runtime-neutral abstraction for referencing a pre-existing named volume:
 * - Kubernetes: maps to a PersistentVolumeClaim in the same namespace.
 * - Docker: maps to a Docker named volume.
 */
export interface PVC extends Record<string, unknown> {
  /**
   * Name of the platform volume.
   * In Kubernetes this is the PVC name; in Docker this is the named volume name.
   */
  claimName: string;
  /**
   * When true (default), auto-create the volume if it does not exist.
   */
  createIfNotExists?: boolean;
  /**
   * When true, delete auto-created volume on sandbox deletion (Docker-only).
   */
  deleteOnSandboxTermination?: boolean;
  /**
   * Kubernetes StorageClass name for auto-created PVCs.
   * Null means use cluster default. Ignored for Docker.
   */
  storageClass?: string | null;
  /**
   * Capacity request for auto-created PVCs (e.g. "1Gi").
   * Ignored for Docker.
   */
  storage?: string | null;
  /**
   * Access modes for auto-created PVCs (e.g. ["ReadWriteOnce"]).
   * Ignored for Docker.
   */
  accessModes?: string[] | null;
}

/**
 * Alibaba Cloud OSS mount backend via ossfs.
 *
 * The runtime mounts a host-side OSS path under `storage.ossfs_mount_root`
 * so the container sees the bucket contents at the specified mount path.
 *
 * In Docker runtime, OSSFS backend requires OpenSandbox Server to run on a Linux host with FUSE support.
 */
export interface OSSFS extends Record<string, unknown> {
  /**
   * OSS bucket name.
   */
  bucket: string;
  /**
   * OSS endpoint (e.g., "oss-cn-hangzhou.aliyuncs.com").
   */
  endpoint: string;
  /**
   * ossfs major version used by runtime mount integration.
   * @default "2.0"
   */
  version?: "1.0" | "2.0";
  /**
   * Additional ossfs mount options.
   *
   * - `1.0`: mounts with `ossfs ... -o <option>`
   * - `2.0`: mounts with `ossfs2 mount ... -c <config-file>` and encodes options as `--<option>` lines in the config file
   */
  options?: string[];
  /**
   * OSS access key ID for inline credentials mode.
   */
  accessKeyId: string;
  /**
   * OSS access key secret for inline credentials mode.
   */
  accessKeySecret: string;
}

/**
 * Storage mount definition for a sandbox.
 *
 * Each volume entry contains:
 * - A unique name identifier
 * - Exactly one backend (host, pvc, ossfs) with backend-specific fields
 * - Common mount settings (mountPath, readOnly, subPath)
 */
export interface Volume extends Record<string, unknown> {
  /**
   * Unique identifier for the volume within the sandbox.
   */
  name: string;
  /**
   * Host path bind mount backend (mutually exclusive with pvc, ossfs).
   */
  host?: Host;
  /**
   * Kubernetes PVC mount backend (mutually exclusive with host, ossfs).
   */
  pvc?: PVC;
  /**
   * Alibaba Cloud OSSFS mount backend (mutually exclusive with host, pvc).
   */
  ossfs?: OSSFS;
  /**
   * Absolute path inside the container where the volume is mounted.
   */
  mountPath: string;
  /**
   * If true, the volume is mounted as read-only. Defaults to false (read-write).
   */
  readOnly?: boolean;
  /**
   * Optional subdirectory under the backend path to mount.
   */
  subPath?: string;
}

export type SandboxState =
  | "Creating"
  | "Running"
  | "Pausing"
  | "Paused"
  | "Resuming"
  | "Deleting"
  | "Deleted"
  | "Error"
  | string;

export interface SandboxStatus extends Record<string, unknown> {
  state: SandboxState;
  reason?: string;
  message?: string;
}

export interface SandboxInfo extends Record<string, unknown> {
  id: SandboxId;
  image?: ImageSpec;
  snapshotId?: string;
  platform?: PlatformSpec;
  entrypoint: string[];
  metadata?: Record<string, string>;
  status: SandboxStatus;
  /**
   * Sandbox creation time.
   */
  createdAt: Date;
  /**
   * Sandbox expiration time (server-side TTL).
   */
  expiresAt: Date | null;
}

export interface CreateSandboxRequest extends Record<string, unknown> {
  image?: ImageSpec;
  snapshotId?: string;
  entrypoint?: string[];
  platform?: PlatformSpec;
  /**
   * Whether to require secure access headers for sandbox endpoint access.
   */
  secureAccess?: boolean;
  /**
   * Timeout in seconds (server semantics).
   */
  timeout?: number | null;
  resourceLimits: ResourceLimits;
  env?: Record<string, string>;
  metadata?: Record<string, string>;
  /**
   * Optional outbound network policy for the sandbox.
   */
  networkPolicy?: NetworkPolicy;
  /**
   * Optional list of volume mounts for persistent storage.
   */
  volumes?: Volume[];
  extensions?: Record<string, unknown>;
}

export interface CreateSandboxResponse extends Record<string, unknown> {
  id: SandboxId;
  status: SandboxStatus;
  platform?: PlatformSpec;
  metadata?: Record<string, string>;
  /**
   * Sandbox expiration time after creation.
   */
  expiresAt: Date | null;
  /**
   * Sandbox creation time.
   */
  createdAt: Date;
  entrypoint: string[];
}

export type SnapshotState = "Creating" | "Deleting" | "Ready" | "Failed" | string;

export interface SnapshotStatus extends Record<string, unknown> {
  state: SnapshotState;
  reason?: string;
  message?: string;
  lastTransitionAt?: Date;
}

export interface SnapshotInfo extends Record<string, unknown> {
  id: string;
  sandboxId: SandboxId;
  name?: string;
  status: SnapshotStatus;
  createdAt: Date;
}

export interface CreateSnapshotRequest extends Record<string, unknown> {
  name?: string;
}

export interface ListSnapshotsResponse extends Record<string, unknown> {
  items: SnapshotInfo[];
  pagination?: PaginationInfo;
}

export interface PaginationInfo extends Record<string, unknown> {
  page: number;
  pageSize: number;
  totalItems: number;
  totalPages: number;
  hasNextPage: boolean;
}

export interface ListSandboxesResponse extends Record<string, unknown> {
  items: SandboxInfo[];
  pagination?: PaginationInfo;
}

export type SandboxMetadataPatch = Record<string, string | null>;

export interface RenewSandboxExpirationRequest {
  expiresAt: string;
}

export interface RenewSandboxExpirationResponse extends Record<string, unknown> {
  /**
   * Updated expiration time (if the server returns it).
   */
  expiresAt?: Date;
}

export interface Endpoint extends Record<string, unknown> {
  endpoint: string;
  /**
   * Headers that must be included on every request targeting this endpoint
   * (e.g. when the server requires them for routing or auth). Omit or empty if not required.
   */
  headers?: Record<string, string>;
}

export interface ListSandboxesParams {
  /**
   * Filter by lifecycle state (the API supports multiple `state` query params).
   * Example: `{ states: ["Running", "Paused"] }`
   */
  states?: string[];
  /**
   * Filter by metadata key-value pairs.
   * NOTE: This will be encoded to a single `metadata` query parameter as described in the spec.
   */
  metadata?: Record<string, string>;
  page?: number;
  pageSize?: number;
}

export interface ListSnapshotsParams {
  sandboxId?: SandboxId;
  states?: string[];
  page?: number;
  pageSize?: number;
}
