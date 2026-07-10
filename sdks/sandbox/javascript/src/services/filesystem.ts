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

import type { SearchFilesResponse } from "../models/filesystem.js";
import type {
  ContentReplaceEntry,
  ContentReplaceResult,
  DirectoryListEntry,
  FileInfo,
  MoveEntry,
  SearchEntry,
  SetPermissionEntry,
  WriteEntry,
} from "../models/filesystem.js";

/**
 * High-level filesystem facade (JS-friendly).
 *
 * This interface provides a convenience layer over the underlying execd filesystem API:
 * it offers common operations (read/write/search/move/delete) and supports streaming I/O for large files.
 */
export interface SandboxFiles {
  getFileInfo(paths: string[]): Promise<Record<string, FileInfo>>;
  search(entry: SearchEntry): Promise<SearchFilesResponse>;
  listDirectory(entry: DirectoryListEntry): Promise<FileInfo[]>;

  createDirectories(entries: Pick<WriteEntry, "path" | "mode" | "owner" | "group">[]): Promise<void>;
  deleteDirectories(paths: string[]): Promise<void>;

  writeFiles(entries: WriteEntry[]): Promise<void>;
  readFile(path: string, opts?: { encoding?: string; range?: string; offset?: number; limit?: number }): Promise<string>;
  readBytes(path: string, opts?: { range?: string; offset?: number; limit?: number }): Promise<Uint8Array>;
  readBytesStream(path: string, opts?: { range?: string; offset?: number; limit?: number }): AsyncIterable<Uint8Array>;

  deleteFiles(paths: string[]): Promise<void>;
  moveFiles(entries: MoveEntry[]): Promise<void>;
  replaceContents(entries: ContentReplaceEntry[]): Promise<void>;
  replaceContentsDetailed(entries: ContentReplaceEntry[]): Promise<ContentReplaceResult[]>;
  setPermissions(entries: SetPermissionEntry[]): Promise<void>;
}