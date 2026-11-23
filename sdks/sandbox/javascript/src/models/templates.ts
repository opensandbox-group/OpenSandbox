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

/**
 * Domain models for fsb (fast-sandbox) golden-image template management.
 *
 * Templates declare an asynchronous golden-image build; only `Succeeded`
 * templates can back template-based sandbox creation. Template management
 * requires a Kubernetes-backed runtime.
 */

import type { PaginationInfo, ResourceLimits } from "./sandboxes.js";

/**
 * Storage encoding of the produced snapshot set.
 */
export type TemplateFormat = "native" | "overlaybd";

/**
 * High-level lifecycle phase of a fsb template build.
 *
 * The server may introduce new phases in future versions; handle unknown
 * values gracefully.
 */
export type TemplatePhase = "Pending" | "Building" | "Succeeded" | "Failed" | string;

export interface TemplateReadiness extends Record<string, unknown> {
  /**
   * Readiness probe checked first during the golden-image build;
   * e.g. `tcp://127.0.0.1:44772` or `cmd://<command>`.
   */
  probe?: string;
  /**
   * Fallback warmup window in seconds (default 60).
   */
  warmupSeconds?: number;
}

export interface TemplateStatus extends Record<string, unknown> {
  /**
   * Build lifecycle phase.
   */
  phase: TemplatePhase;
  /**
   * Published S3 manifest reference or immutable OCI reference; present when the phase is `Succeeded`.
   */
  manifestRef?: string;
  /**
   * Failure reason when the phase is `Failed`.
   */
  message?: string;
}

export interface CreateTemplateRequest extends Record<string, unknown> {
  /**
   * Source OCI image reference the golden image is built from.
   */
  image: string;
  /**
   * Publish destination: `s3://bucket/publish` (native) or `oci://registry/repository` (overlaybd, no tag or digest).
   */
  publish: string;
  /**
   * Guest machine sizing, e.g. `{ cpu: "1", memory: "512Mi", disk: "2Gi" }`.
   * Defaults when omitted: cpu `1`, memory `512Mi`, disk `2Gi`.
   */
  resourceLimits?: ResourceLimits;
  /**
   * Guest business command (argv); empty defaults to `["tail", "-f", "/dev/null"]`.
   */
  entrypoint?: string[];
  /**
   * Environment variables baked into the golden image (injected as
   * `/etc/sandbox-init.env` in the guest; literal values only). The source
   * image's own OCI `Config.Env` is inherited; an env with the same name
   * overrides the inherited value. Names must be valid shell variable names.
   */
  env?: Record<string, string>;
  /**
   * Custom key-value metadata for management, filtering, and tagging.
   */
  metadata?: Record<string, string>;
  /**
   * Build-side readiness gate.
   */
  readiness?: TemplateReadiness;
  /**
   * Storage encoding of the produced snapshot set. Defaults to `overlaybd`.
   */
  format?: TemplateFormat;
}

export interface TemplateInfo extends Record<string, unknown> {
  /**
   * Server-generated template ID (`tpl_<uuid>`).
   */
  templateId: string;
  /**
   * Source OCI image reference.
   */
  image: string;
  /**
   * S3-compatible or repository-only OCI publish target.
   */
  publish: string;
  /**
   * Snapshot storage encoding.
   */
  format: TemplateFormat;
  /**
   * Current build status.
   */
  status: TemplateStatus;
  /**
   * Creation timestamp (RFC 3339 UTC).
   */
  createdAt: Date;
  /**
   * Last update timestamp (RFC 3339 UTC).
   */
  updatedAt: Date;
  /**
   * Guest machine sizing (cpu/memory/disk).
   */
  resourceLimits?: ResourceLimits;
  /**
   * Guest business command (argv).
   */
  entrypoint?: string[];
  /**
   * Environment variables baked into the golden image.
   */
  env?: Record<string, string>;
  /**
   * Custom metadata from the creation request.
   */
  metadata?: Record<string, string>;
  /**
   * Build-side readiness gate.
   */
  readiness?: TemplateReadiness;
}

export interface ListTemplatesParams {
  /**
   * Filter by metadata key-value pairs (AND logic).
   * NOTE: This will be encoded to a single `metadata` query parameter as described in the spec.
   */
  metadata?: Record<string, string>;
  /**
   * Pagination page number (1-indexed).
   */
  page?: number;
  /**
   * Number of items per page (1-200).
   */
  pageSize?: number;
}

export interface ListTemplatesResponse extends Record<string, unknown> {
  items: TemplateInfo[];
  pagination?: PaginationInfo;
}
