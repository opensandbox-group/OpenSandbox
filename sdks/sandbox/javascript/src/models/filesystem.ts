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

export type FileEntryType = "file" | "directory" | "symlink" | "other";

export interface FileInfo extends Record<string, unknown> {
  path: string;
  type?: FileEntryType;
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

export interface ByteRange {
  /** Inclusive first byte offset, or -1 if parsing failed. */
  start: number;
  /** Inclusive last byte offset, or -1 if parsing failed. */
  end: number;
  /** Complete file size, or -1 if unknown or parsing failed. */
  total: number;
  /** Original Content-Range header value. */
  raw: string;
}

export interface ReadBytesResponse<TBody> {
  body: TBody;
  statusCode: number;
  contentType?: string;
  contentDisposition?: string;
  /** Response body size, or -1 if the Content-Length header is unavailable. */
  contentLength: number;
  /** Complete file size, or -1 if unknown. */
  totalSize: number;
  contentRange?: ByteRange;
  /** Whether the server returned 206 Partial Content. */
  isPartial: boolean;
}

/** A single-use download body that can be closed without consuming it. */
export interface ReadBytesStream extends AsyncIterable<Uint8Array> {
  close(): Promise<void>;
}

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

export interface DirectoryListEntry {
  path: string;
  depth?: number;
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
