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

import type { ExecdClient } from "../openapi/execdClient.js";
import { throwOnOpenApiFetchError } from "./openapiError.js";
import type { SandboxFiles } from "../services/filesystem.js";
import type { paths as ExecdPaths } from "../api/execd.js";
import type {
  ContentReplaceEntry,
  ContentReplaceResult,
  DirectoryListEntry,
  FileInfo,
  FileMetadata,
  FilesInfoResponse,
  MoveEntry,
  Permission,
  RenameFileItem,
  ReplaceFileContentItem,
  SearchEntry,
  SearchFilesResponse,
  SetPermissionEntry,
  WriteEntry,
} from "../models/filesystem.js";
import { SandboxApiException, SandboxError } from "../core/exceptions.js";

function joinUrl(baseUrl: string, pathname: string): string {
  const base = baseUrl.endsWith("/") ? baseUrl.slice(0, -1) : baseUrl;
  const path = pathname.startsWith("/") ? pathname : `/${pathname}`;
  return `${base}${path}`;
}

function toUploadBlob(data: Blob | Uint8Array | ArrayBuffer | string): Blob {
  if (typeof data === "string") return new Blob([data]);
  if (data instanceof Blob) return data;
  if (data instanceof ArrayBuffer) return new Blob([data]);
  // Copy into a new Uint8Array backed by ArrayBuffer (not SharedArrayBuffer)
  const copied = Uint8Array.from(data);
  return new Blob([copied.buffer]);
}

function isReadableStream(v: unknown): v is ReadableStream<Uint8Array> {
  return !!v && typeof (v as any).getReader === "function";
}

function isAsyncIterable(v: unknown): v is AsyncIterable<Uint8Array> {
  return !!v && typeof (v as any)[Symbol.asyncIterator] === "function";
}

function isNodeRuntime(): boolean {
  const p = (globalThis as any)?.process;
  return !!(p?.versions?.node);
}

async function collectBytes(
  source: ReadableStream<Uint8Array> | AsyncIterable<Uint8Array>
): Promise<Uint8Array> {
  const chunks: Uint8Array[] = [];
  let total = 0;

  if (isReadableStream(source)) {
    const reader = source.getReader();
    try {
      while (true) {
        const { done, value } = await reader.read();
        if (done) break;
        if (value) {
          chunks.push(value);
          total += value.length;
        }
      }
    } finally {
      reader.releaseLock();
    }
  } else {
    for await (const chunk of source) {
      chunks.push(chunk);
      total += chunk.length;
    }
  }

  const out = new Uint8Array(total);
  let offset = 0;
  for (const chunk of chunks) {
    out.set(chunk, offset);
    offset += chunk.length;
  }
  return out;
}

function toReadableStream(
  it: AsyncIterable<Uint8Array>
): ReadableStream<Uint8Array> {
  const RS: any = ReadableStream as any;
  if (typeof RS?.from === "function") return RS.from(it);
  const iterator = it[Symbol.asyncIterator]();
  return new ReadableStream<Uint8Array>({
    async pull(controller) {
      const r = await iterator.next();
      if (r.done) {
        controller.close();
        return;
      }
      controller.enqueue(r.value);
    },
    async cancel() {
      await iterator.return?.();
    },
  });
}

function basename(p: string): string {
  const parts = p.split("/").filter(Boolean);
  return parts.length ? parts[parts.length - 1] : "file";
}

function encodeUtf8(s: string): Uint8Array {
  return new TextEncoder().encode(s);
}

async function* multipartUploadBody(opts: {
  boundary: string;
  metadataJson: string;
  fileName: string;
  fileContentType: string;
  file: ReadableStream<Uint8Array> | AsyncIterable<Uint8Array>;
}): AsyncIterable<Uint8Array> {
  const b = opts.boundary;

  // Part 1: metadata (application/json)
  yield encodeUtf8(`--${b}\r\n`);
  yield encodeUtf8(
    `Content-Disposition: form-data; name="metadata"; filename="metadata"\r\n`
  );
  yield encodeUtf8(`Content-Type: application/json\r\n\r\n`);
  yield encodeUtf8(opts.metadataJson);
  yield encodeUtf8(`\r\n`);

  // Part 2: file
  yield encodeUtf8(`--${b}\r\n`);
  yield encodeUtf8(
    `Content-Disposition: form-data; name="file"; filename="${opts.fileName}"\r\n`
  );
  yield encodeUtf8(`Content-Type: ${opts.fileContentType}\r\n\r\n`);

  if (isReadableStream(opts.file)) {
    const reader = opts.file.getReader();
    try {
      while (true) {
        const { done, value } = await reader.read();
        if (done) break;
        if (value) yield value;
      }
    } finally {
      reader.releaseLock();
    }
  } else {
    for await (const chunk of opts.file) {
      yield chunk;
    }
  }

  yield encodeUtf8(`\r\n--${b}--\r\n`);
}

export interface FilesystemAdapterOptions {
  /**
   * Must match the baseUrl used by the ExecdClient, used for binary endpoints
   * like download/upload where we bypass JSON parsing.
   */
  baseUrl: string;
  fetch?: typeof fetch;
  headers?: Record<string, string>;
}

function toPermission(e: {
  mode?: number;
  owner?: string;
  group?: string;
}): Permission {
  return {
    mode: e.mode ?? 755,
    owner: e.owner,
    group: e.group,
  } as Permission;
}

/**
 * Filesystem adapter that exposes user-facing file APIs (`sandbox.files`).
 *
 * This adapter owns all request/response conversions:
 * - Maps friendly method shapes to API payloads
 * - Parses timestamps into `Date`
 * - Implements streaming upload/download helpers
 */
export class FilesystemAdapter implements SandboxFiles {
  private readonly fetch: typeof fetch;

  private static readonly Api = {
    // This is intentionally derived from OpenAPI schema types so API changes surface quickly.
    SearchFilesOk:
      null as unknown as ExecdPaths["/files/search"]["get"]["responses"][200]["content"]["application/json"],
    FilesInfoOk:
      null as unknown as ExecdPaths["/files/info"]["get"]["responses"][200]["content"]["application/json"],
    ListDirectoryOk:
      null as unknown as ExecdPaths["/directories/list"]["get"]["responses"][200]["content"]["application/json"],
    MakeDirsRequest:
      null as unknown as ExecdPaths["/directories"]["post"]["requestBody"]["content"]["application/json"],
    SetPermissionsRequest:
      null as unknown as ExecdPaths["/files/permissions"]["post"]["requestBody"]["content"]["application/json"],
    MoveFilesRequest:
      null as unknown as ExecdPaths["/files/mv"]["post"]["requestBody"]["content"]["application/json"],
    ReplaceContentsRequest:
      null as unknown as ExecdPaths["/files/replace"]["post"]["requestBody"]["content"]["application/json"],
    ReplaceContentsOk:
      null as unknown as ExecdPaths["/files/replace"]["post"]["responses"][200]["content"]["application/json"],
  };

  constructor(
    private readonly client: ExecdClient,
    private readonly opts: FilesystemAdapterOptions
  ) {
    this.fetch = opts.fetch ?? fetch;
  }

  private parseIsoDate(field: string, v: unknown): Date {
    if (typeof v !== "string" || !v) {
      throw new Error(`Invalid ${field}: expected ISO string, got ${typeof v}`);
    }
    const d = new Date(v);
    if (Number.isNaN(d.getTime())) {
      throw new Error(`Invalid ${field}: ${v}`);
    }
    return d;
  }

  private static readonly _ApiFileInfo =
    null as unknown as (typeof FilesystemAdapter.Api.SearchFilesOk)[number];

  private mapApiFileInfo(raw: typeof FilesystemAdapter._ApiFileInfo): FileInfo {
    const { path, type, size, created_at, modified_at, mode, owner, group, ...rest } =
      raw;

    return {
      ...rest,
      path,
      type,
      size,
      mode,
      owner,
      group,
      createdAt: created_at
        ? this.parseIsoDate("createdAt", created_at)
        : undefined,
      modifiedAt: modified_at
        ? this.parseIsoDate("modifiedAt", modified_at)
        : undefined,
    };
  }

  async getFileInfo(paths: string[]): Promise<Record<string, FileInfo>> {
    const { data, error, response } = await this.client.GET("/files/info", {
      params: { query: { path: paths } },
    });
    throwOnOpenApiFetchError({ error, response }, "Get file info failed");
    const raw = data as typeof FilesystemAdapter.Api.FilesInfoOk | undefined;
    if (!raw) return {} as FilesInfoResponse;
    if (typeof raw !== "object") {
      throw new Error(
        `Get file info failed: unexpected response shape (got ${typeof raw})`
      );
    }
    const out: Record<string, FileInfo> = {};
    for (const [k, v] of Object.entries(raw as Record<string, unknown>)) {
      if (!v || typeof v !== "object") {
        throw new Error(
          `Get file info failed: invalid file info for path=${k}`
        );
      }
      out[k] = this.mapApiFileInfo(v as typeof FilesystemAdapter._ApiFileInfo);
    }
    return out as FilesInfoResponse;
  }

  async deleteFiles(paths: string[]): Promise<void> {
    const { error, response } = await this.client.DELETE("/files", {
      params: { query: { path: paths } },
    });
    throwOnOpenApiFetchError({ error, response }, "Delete files failed");
  }

  async createDirectories(
    entries: Pick<WriteEntry, "path" | "mode" | "owner" | "group">[]
  ): Promise<void> {
    const map: Record<string, Permission> = {};
    for (const e of entries) {
      map[e.path] = toPermission(e);
    }
    const body = map as unknown as typeof FilesystemAdapter.Api.MakeDirsRequest;
    const { error, response } = await this.client.POST("/directories", {
      body,
    });
    throwOnOpenApiFetchError({ error, response }, "Create directories failed");
  }

  async deleteDirectories(paths: string[]): Promise<void> {
    const { error, response } = await this.client.DELETE("/directories", {
      params: { query: { path: paths } },
    });
    throwOnOpenApiFetchError({ error, response }, "Delete directories failed");
  }

  async listDirectory(entry: DirectoryListEntry): Promise<FileInfo[]> {
    const { data, error, response } = await this.client.GET("/directories/list", {
      params: { query: { path: entry.path, depth: entry.depth } },
    });
    throwOnOpenApiFetchError({ error, response }, "List directory failed");

    const ok = data as typeof FilesystemAdapter.Api.ListDirectoryOk | undefined;
    if (!ok) return [];
    if (!Array.isArray(ok)) {
      throw new Error(
        `List directory failed: unexpected response shape (expected array, got ${typeof ok})`
      );
    }
    return ok.map((x) => this.mapApiFileInfo(x));
  }

  async setPermissions(entries: SetPermissionEntry[]): Promise<void> {
    const req: Record<string, Permission> = {};
    for (const e of entries) {
      req[e.path] = toPermission(e);
    }
    const body =
      req as unknown as typeof FilesystemAdapter.Api.SetPermissionsRequest;
    const { error, response } = await this.client.POST("/files/permissions", {
      body,
    });
    throwOnOpenApiFetchError({ error, response }, "Set permissions failed");
  }

  async moveFiles(entries: MoveEntry[]): Promise<void> {
    const req: RenameFileItem[] = entries.map((e) => ({
      src: e.src,
      dest: e.dest,
    }));
    const body =
      req as unknown as typeof FilesystemAdapter.Api.MoveFilesRequest;
    const { error, response } = await this.client.POST("/files/mv", {
      body,
    });
    throwOnOpenApiFetchError({ error, response }, "Move files failed");
  }

  async replaceContents(entries: ContentReplaceEntry[]): Promise<void> {
    const req: Record<string, ReplaceFileContentItem> = {};
    for (const e of entries) {
      req[e.path] = { old: e.oldContent, new: e.newContent };
    }
    const body =
      req as unknown as typeof FilesystemAdapter.Api.ReplaceContentsRequest;
    const { error, response } = await this.client.POST("/files/replace", {
      body,
    });
    throwOnOpenApiFetchError({ error, response }, "Replace contents failed");
  }

  async replaceContentsDetailed(entries: ContentReplaceEntry[]): Promise<ContentReplaceResult[]> {
    const req: Record<string, ReplaceFileContentItem> = {};
    for (const e of entries) {
      req[e.path] = { old: e.oldContent, new: e.newContent };
    }
    const body =
      req as unknown as typeof FilesystemAdapter.Api.ReplaceContentsRequest;
    const { data, error, response } = await this.client.POST("/files/replace", {
      params: { query: { verbose: true } },
      body,
    });
    throwOnOpenApiFetchError({ error, response }, "Replace contents failed");

    const ok = data as typeof FilesystemAdapter.Api.ReplaceContentsOk | undefined;
    if (!ok) return [];
    return Object.entries(ok).map(([path, result]) => ({
      path,
      replacedCount: result.replacedCount,
    }));
  }

  async search(entry: SearchEntry): Promise<SearchFilesResponse> {
    const { data, error, response } = await this.client.GET("/files/search", {
      params: { query: { path: entry.path, pattern: entry.pattern } },
    });
    throwOnOpenApiFetchError({ error, response }, "Search files failed");

    // Make the OpenAPI contract explicit (and fail loudly on unexpected shapes).
    const ok = data as typeof FilesystemAdapter.Api.SearchFilesOk | undefined;
    if (!ok) return [];
    if (!Array.isArray(ok)) {
      throw new Error(
        `Search files failed: unexpected response shape (expected array, got ${typeof ok})`
      );
    }
    return ok.map((x) => this.mapApiFileInfo(x));
  }

  private async uploadFile(
    meta: FileMetadata,
    data:
      | Blob
      | Uint8Array
      | ArrayBuffer
      | string
      | AsyncIterable<Uint8Array>
      | ReadableStream<Uint8Array>
  ): Promise<void> {
    const url = joinUrl(this.opts.baseUrl, "/files/upload");
    const fileName = basename(meta.path);
    const metadataJson = JSON.stringify(meta);

    // Streaming path (large files): build multipart body manually to avoid buffering.
    if (isReadableStream(data) || isAsyncIterable(data)) {
      // Browsers do not allow streaming multipart requests with custom boundaries.
      // Fall back to in-memory uploads when streaming is unavailable.
      if (!isNodeRuntime()) {
        const bytes = await collectBytes(data);
        return await this.uploadFile(meta, bytes);
      }
      const boundary = `opensandbox_${Math.random()
        .toString(16)
        .slice(2)}_${Date.now()}`;
      const bodyIt = multipartUploadBody({
        boundary,
        metadataJson,
        fileName,
        fileContentType: "application/octet-stream",
        file: data,
      });
      const stream = toReadableStream(bodyIt);

      const res = await this.fetch(url, {
        method: "POST",
        headers: {
          "content-type": `multipart/form-data; boundary=${boundary}`,
          ...(this.opts.headers ?? {}),
        },
        body: stream as any,
        // Node fetch (undici) requires duplex for streaming request bodies.
        duplex: "half" as any,
      } as any);

      if (!res.ok) {
        const requestId = res.headers.get("x-request-id") ?? undefined;
        const rawBody = await res.text().catch(() => undefined);
        throw new SandboxApiException({
          message: `Upload failed (status=${res.status})`,
          statusCode: res.status,
          requestId,
          error: new SandboxError(
            SandboxError.UNEXPECTED_RESPONSE,
            "Upload failed"
          ),
          rawBody,
        });
      }
      return;
    }

    // In-memory path (small files): use FormData.
    const form = new FormData();
    form.append(
      "metadata",
      new Blob([metadataJson], { type: "application/json" }),
      "metadata"
    );

    if (typeof data === "string") {
      const textBlob = new Blob([data], { type: "text/plain; charset=utf-8" });
      form.append("file", textBlob, fileName);
    } else {
      const blob = toUploadBlob(data);
      const fileBlob = blob.type
        ? blob
        : new Blob([blob], { type: "application/octet-stream" });
      form.append("file", fileBlob, fileName);
    }

    const res = await this.fetch(url, {
      method: "POST",
      headers: {
        ...(this.opts.headers ?? {}),
      },
      body: form,
    });

    if (!res.ok) {
      const requestId = res.headers.get("x-request-id") ?? undefined;
      const rawBody = await res.text().catch(() => undefined);
      throw new SandboxApiException({
        message: `Upload failed (status=${res.status})`,
        statusCode: res.status,
        requestId,
        error: new SandboxError(
          SandboxError.UNEXPECTED_RESPONSE,
          "Upload failed"
        ),
        rawBody,
      });
    }
  }

  async readBytes(
    path: string,
    opts?: { range?: string; offset?: number; limit?: number }
  ): Promise<Uint8Array> {
    let url =
      joinUrl(this.opts.baseUrl, "/files/download") +
      `?path=${encodeURIComponent(path)}`;
    if (opts?.offset != null) url += `&offset=${opts.offset}`;
    if (opts?.limit != null) url += `&limit=${opts.limit}`;
    const res = await this.fetch(url, {
      method: "GET",
      headers: {
        ...(this.opts.headers ?? {}),
        ...(opts?.range ? { Range: opts.range } : {}),
      },
    });
    if (!res.ok) {
      const requestId = res.headers.get("x-request-id") ?? undefined;
      const rawBody = await res.text().catch(() => undefined);
      throw new SandboxApiException({
        message: "Download failed",
        statusCode: res.status,
        requestId,
        error: new SandboxError(
          SandboxError.UNEXPECTED_RESPONSE,
          "Download failed"
        ),
        rawBody,
      });
    }
    const ab = await res.arrayBuffer();
    return new Uint8Array(ab);
  }

  readBytesStream(
    path: string,
    opts?: { range?: string; offset?: number; limit?: number }
  ): AsyncIterable<Uint8Array> {
    return this.downloadStream(path, opts);
  }

  private async *downloadStream(
    path: string,
    opts?: { range?: string; offset?: number; limit?: number }
  ): AsyncIterable<Uint8Array> {
    let url =
      joinUrl(this.opts.baseUrl, "/files/download") +
      `?path=${encodeURIComponent(path)}`;
    if (opts?.offset != null) url += `&offset=${opts.offset}`;
    if (opts?.limit != null) url += `&limit=${opts.limit}`;
    const res = await this.fetch(url, {
      method: "GET",
      headers: {
        ...(this.opts.headers ?? {}),
        ...(opts?.range ? { Range: opts.range } : {}),
      },
    });
    if (!res.ok) {
      const requestId = res.headers.get("x-request-id") ?? undefined;
      const rawBody = await res.text().catch(() => undefined);
      throw new SandboxApiException({
        message: "Download stream failed",
        statusCode: res.status,
        requestId,
        error: new SandboxError(
          SandboxError.UNEXPECTED_RESPONSE,
          "Download stream failed"
        ),
        rawBody,
      });
    }

    const body = res.body as ReadableStream<Uint8Array> | null;
    if (!body) return;
    const reader = body.getReader();
    while (true) {
      const { done, value } = await reader.read();
      if (done) return;
      if (value) yield value;
    }
  }

  async readFile(
    path: string,
    opts?: { encoding?: string; range?: string; offset?: number; limit?: number }
  ): Promise<string> {
    const bytes = await this.readBytes(path, { range: opts?.range, offset: opts?.offset, limit: opts?.limit });
    const encoding = opts?.encoding ?? "utf-8";
    return new TextDecoder(encoding).decode(bytes);
  }

  async writeFiles(entries: WriteEntry[]): Promise<void> {
    for (const e of entries) {
      const meta: FileMetadata = {
        path: e.path,
        owner: e.owner,
        group: e.group,
        mode: e.mode,
      };
      await this.uploadFile(meta, e.data ?? "");
    }
  }
}