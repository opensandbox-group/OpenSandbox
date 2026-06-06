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
 * Domain models for filesystem.
 *
 * IMPORTANT:
 * - These are NOT OpenAPI-generated types.
 * - They are intentionally stable and JS-friendly.
 */

export interface FileInfo extends Record<string, unknown> {
  path: string;
  size?: number;
  /**
   * Last modification time.
   */
  modifiedAt?: Date;
  /**
   * Creation time.
   */
  createdAt?: Date;
  mode?: number;
  owner?: string;
  group?: string;
}

export interface Permission extends Record<string, unknown> {
  mode: number;
  owner?: string;
  group?: string;
}

export interface FileMetadata extends Record<string, unknown> {
  path: string;
  mode?: number;
  owner?: string;
  group?: string;
}

export interface RenameFileItem extends Record<string, unknown> {
  src: string;
  dest: string;
}

export interface ReplaceFileContentItem extends Record<string, unknown> {
  old: string;
  new: string;
}

export type FilesInfoResponse = Record<string, FileInfo>;

export type SearchFilesResponse = FileInfo[];

// High-level filesystem facade models used by `sandbox.files`.
export interface WriteEntry {
  path: string;
  /**
   * File data to upload.
   *
   * Supports:
   * - string / bytes / Blob (in-memory)
   * - AsyncIterable<Uint8Array> or ReadableStream<Uint8Array> (streaming upload for large files)
   */
  data?: string | Uint8Array | ArrayBuffer | Blob | AsyncIterable<Uint8Array> | ReadableStream<Uint8Array>;
  mode?: number;
  owner?: string;
  group?: string;
}

export interface SearchEntry {
  path: string;
  pattern?: string;
}

export interface MoveEntry {
  src: string;
  dest: string;
}

export interface ContentReplaceEntry {
  path: string;
  oldContent: string;
  newContent: string;
}

export interface ContentReplaceResult {
  path: string;
  replacedCount: number;
}

export interface SetPermissionEntry {
  path: string;
  mode: number;
  owner?: string;
  group?: string;
}