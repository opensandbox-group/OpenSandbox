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
 * Isolated filesystem adapter using auto-generated typed API paths.
 *
 * Uses openapi-fetch client with explicit sessionId path parameter instead
 * of URL prefix composition.
 */

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
  const copied = Uint8Array.from(data);
  return new Blob([copied.buffer]);
}

function isReadableStream(v: unknown): v is ReadableStream<Uint8Array> {
  return !!v && typeof (v as any).getReader === "function";
}

function isAsyncIterable(v: unknown): v is AsyncIterable<Uint8Array> {
  return !!v && typeof (v as any)[Symbol.asyncIterator] === "function";
}

async function collectBytes(
  source: ReadableStream<Uint8Array> | AsyncIterable<Uint8Array>,
): Promise<Uint8Array> {
  const chunks: Uint8Array[] = [];
  let total = 0;
  if (isReadableStream(source)) {
    const reader = source.getReader();
    try {
      while (true) {
        const { done, value } = await reader.read();
        if (done) break;
        if (value) { chunks.push(value); total += value.length; }
      }
    } finally { reader.releaseLock(); }
  } else {
    for await (const chunk of source) {
      chunks.push(chunk);
      total += chunk.length;
    }
  }
  const out = new Uint8Array(total);
  let offset = 0;
  for (const chunk of chunks) { out.set(chunk, offset); offset += chunk.length; }
  return out;
}

function basename(p: string): string {
  const parts = p.split("/").filter(Boolean);
  return parts.length ? parts[parts.length - 1] : "file";
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

export interface IsolatedFilesystemAdapterOptions {
  baseUrl: string;
  sessionId: string;
  fetch?: typeof fetch;
  headers?: Record<string, string>;
}

export class IsolatedFilesystemAdapter implements SandboxFiles {
  private readonly fetch: typeof fetch;
  private readonly sessionId: string;

  private static readonly Api = {
    SearchFilesOk:
      null as unknown as ExecdPaths["/v1/isolated/session/{sessionId}/files/search"]["get"]["responses"][200]["content"]["application/json"],
    FilesInfoOk:
      null as unknown as ExecdPaths["/v1/isolated/session/{sessionId}/files/info"]["get"]["responses"][200]["content"]["application/json"],
    ListDirectoryOk:
      null as unknown as ExecdPaths["/v1/isolated/session/{sessionId}/directories/list"]["get"]["responses"][200]["content"]["application/json"],
  };

  constructor(
    private readonly client: ExecdClient,
    private readonly opts: IsolatedFilesystemAdapterOptions,
  ) {
    this.fetch = opts.fetch ?? fetch;
    this.sessionId = opts.sessionId;
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
    null as unknown as (typeof IsolatedFilesystemAdapter.Api.SearchFilesOk)[number];

  private mapApiFileInfo(raw: typeof IsolatedFilesystemAdapter._ApiFileInfo): FileInfo {
    const { path, type, size, created_at, modified_at, mode, owner, group, ...rest } = raw;
    return {
      ...rest,
      path,
      type,
      size,
      mode,
      owner,
      group,
      createdAt: created_at ? this.parseIsoDate("createdAt", created_at) : undefined,
      modifiedAt: modified_at ? this.parseIsoDate("modifiedAt", modified_at) : undefined,
    };
  }

  async getFileInfo(paths: string[]): Promise<Record<string, FileInfo>> {
    const { data, error, response } = await this.client.GET(
      "/v1/isolated/session/{sessionId}/files/info",
      { params: { path: { sessionId: this.sessionId }, query: { path: paths } } },
    );
    throwOnOpenApiFetchError({ error, response }, "Get file info failed");
    const raw = data as typeof IsolatedFilesystemAdapter.Api.FilesInfoOk | undefined;
    if (!raw) return {} as FilesInfoResponse;
    if (typeof raw !== "object") {
      throw new Error(`Get file info failed: unexpected response shape (got ${typeof raw})`);
    }
    const out: Record<string, FileInfo> = {};
    for (const [k, v] of Object.entries(raw as Record<string, unknown>)) {
      if (!v || typeof v !== "object") {
        throw new Error(`Get file info failed: invalid file info for path=${k}`);
      }
      out[k] = this.mapApiFileInfo(v as typeof IsolatedFilesystemAdapter._ApiFileInfo);
    }
    return out as FilesInfoResponse;
  }

  async deleteFiles(paths: string[]): Promise<void> {
    const { error, response } = await this.client.DELETE(
      "/v1/isolated/session/{sessionId}/files",
      { params: { path: { sessionId: this.sessionId }, query: { path: paths } } },
    );
    throwOnOpenApiFetchError({ error, response }, "Delete files failed");
  }

  async createDirectories(
    entries: Pick<WriteEntry, "path" | "mode" | "owner" | "group">[],
  ): Promise<void> {
    const map: Record<string, Permission> = {};
    for (const e of entries) {
      map[e.path] = toPermission(e);
    }
    const { error, response } = await this.client.POST(
      "/v1/isolated/session/{sessionId}/directories",
      {
        params: { path: { sessionId: this.sessionId } },
        body: map as any,
      },
    );
    throwOnOpenApiFetchError({ error, response }, "Create directories failed");
  }

  async deleteDirectories(paths: string[]): Promise<void> {
    const { error, response } = await this.client.DELETE(
      "/v1/isolated/session/{sessionId}/directories",
      { params: { path: { sessionId: this.sessionId }, query: { path: paths } } },
    );
    throwOnOpenApiFetchError({ error, response }, "Delete directories failed");
  }

  async listDirectory(entry: DirectoryListEntry): Promise<FileInfo[]> {
    const { data, error, response } = await this.client.GET(
      "/v1/isolated/session/{sessionId}/directories/list",
      { params: { path: { sessionId: this.sessionId }, query: { path: entry.path, depth: entry.depth } } },
    );
    throwOnOpenApiFetchError({ error, response }, "List directory failed");
    const ok = data as typeof IsolatedFilesystemAdapter.Api.ListDirectoryOk | undefined;
    if (!ok) return [];
    if (!Array.isArray(ok)) {
      throw new Error(`List directory failed: unexpected response shape (expected array, got ${typeof ok})`);
    }
    return ok.map((x) => this.mapApiFileInfo(x));
  }

  async setPermissions(entries: SetPermissionEntry[]): Promise<void> {
    const req: Record<string, Permission> = {};
    for (const e of entries) {
      req[e.path] = toPermission(e);
    }
    const { error, response } = await this.client.POST(
      "/v1/isolated/session/{sessionId}/files/permissions",
      {
        params: { path: { sessionId: this.sessionId } },
        body: req as any,
      },
    );
    throwOnOpenApiFetchError({ error, response }, "Set permissions failed");
  }

  async moveFiles(entries: MoveEntry[]): Promise<void> {
    const req: RenameFileItem[] = entries.map((e) => ({ src: e.src, dest: e.dest }));
    const { error, response } = await this.client.POST(
      "/v1/isolated/session/{sessionId}/files/mv",
      {
        params: { path: { sessionId: this.sessionId } },
        body: req as any,
      },
    );
    throwOnOpenApiFetchError({ error, response }, "Move files failed");
  }

  async replaceContents(entries: ContentReplaceEntry[]): Promise<void> {
    const req: Record<string, ReplaceFileContentItem> = {};
    for (const e of entries) {
      req[e.path] = { old: e.oldContent, new: e.newContent };
    }
    const { error, response } = await this.client.POST(
      "/v1/isolated/session/{sessionId}/files/replace",
      {
        params: { path: { sessionId: this.sessionId } },
        body: req as any,
      },
    );
    throwOnOpenApiFetchError({ error, response }, "Replace contents failed");
  }

  async replaceContentsDetailed(entries: ContentReplaceEntry[]): Promise<ContentReplaceResult[]> {
    const req: Record<string, ReplaceFileContentItem> = {};
    for (const e of entries) {
      req[e.path] = { old: e.oldContent, new: e.newContent };
    }
    const { data, error, response } = await this.client.POST(
      "/v1/isolated/session/{sessionId}/files/replace",
      {
        params: { path: { sessionId: this.sessionId } },
        body: req as any,
      },
    );
    throwOnOpenApiFetchError({ error, response }, "Replace contents failed");
    if (!data) return [];
    return Object.entries(data as Record<string, any>).map(([path, result]) => ({
      path,
      replacedCount: result.replacedCount,
    }));
  }

  async search(entry: SearchEntry): Promise<SearchFilesResponse> {
    const { data, error, response } = await this.client.GET(
      "/v1/isolated/session/{sessionId}/files/search",
      { params: { path: { sessionId: this.sessionId }, query: { path: entry.path, pattern: entry.pattern } } },
    );
    throwOnOpenApiFetchError({ error, response }, "Search files failed");
    const ok = data as typeof IsolatedFilesystemAdapter.Api.SearchFilesOk | undefined;
    if (!ok) return [];
    if (!Array.isArray(ok)) {
      throw new Error(`Search files failed: unexpected response shape (expected array, got ${typeof ok})`);
    }
    return ok.map((x) => this.mapApiFileInfo(x));
  }

  private async uploadFile(
    meta: FileMetadata,
    data: Blob | Uint8Array | ArrayBuffer | string | AsyncIterable<Uint8Array> | ReadableStream<Uint8Array>,
  ): Promise<void> {
    if (isReadableStream(data) || isAsyncIterable(data)) {
      const bytes = await collectBytes(data);
      return await this.uploadFile(meta, bytes);
    }
    const url = joinUrl(
      this.opts.baseUrl,
      `/v1/isolated/session/${encodeURIComponent(this.sessionId)}/files/upload`,
    );
    const fileName = basename(meta.path);
    const metadataJson = JSON.stringify(meta);

    const form = new FormData();
    form.append(
      "metadata",
      new Blob([metadataJson], { type: "application/json" }),
      "metadata",
    );

    if (typeof data === "string") {
      form.append("file", new Blob([data], { type: "text/plain; charset=utf-8" }), fileName);
    } else {
      const blob = toUploadBlob(data);
      const fileBlob = blob.type ? blob : new Blob([blob], { type: "application/octet-stream" });
      form.append("file", fileBlob, fileName);
    }

    const res = await this.fetch(url, {
      method: "POST",
      headers: { ...(this.opts.headers ?? {}) },
      body: form,
    });

    if (!res.ok) {
      const requestId = res.headers.get("x-request-id") ?? undefined;
      const rawBody = await res.text().catch(() => undefined);
      throw new SandboxApiException({
        message: `Upload failed (status=${res.status})`,
        statusCode: res.status,
        requestId,
        error: new SandboxError(SandboxError.UNEXPECTED_RESPONSE, "Upload failed"),
        rawBody,
      });
    }
  }

  async readBytes(
    path: string,
    opts?: { range?: string; offset?: number; limit?: number },
  ): Promise<Uint8Array> {
    let url =
      joinUrl(
        this.opts.baseUrl,
        `/v1/isolated/session/${encodeURIComponent(this.sessionId)}/files/download`,
      ) + `?path=${encodeURIComponent(path)}`;
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
        error: new SandboxError(SandboxError.UNEXPECTED_RESPONSE, "Download failed"),
        rawBody,
      });
    }
    const ab = await res.arrayBuffer();
    return new Uint8Array(ab);
  }

  readBytesStream(
    path: string,
    opts?: { range?: string; offset?: number; limit?: number },
  ): AsyncIterable<Uint8Array> {
    return this.downloadStream(path, opts);
  }

  private async *downloadStream(
    path: string,
    opts?: { range?: string; offset?: number; limit?: number },
  ): AsyncIterable<Uint8Array> {
    let url =
      joinUrl(
        this.opts.baseUrl,
        `/v1/isolated/session/${encodeURIComponent(this.sessionId)}/files/download`,
      ) + `?path=${encodeURIComponent(path)}`;
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
        error: new SandboxError(SandboxError.UNEXPECTED_RESPONSE, "Download stream failed"),
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
    opts?: { encoding?: string; range?: string; offset?: number; limit?: number },
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
