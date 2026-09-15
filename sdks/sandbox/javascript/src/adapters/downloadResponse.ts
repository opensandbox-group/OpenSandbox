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

import type {
  ByteRange,
  ReadBytesResponse,
  ReadBytesStream,
} from "../models/filesystem.js";

function parseHeaderInteger(raw: string | null): number {
  if (raw === null || !/^\d+$/.test(raw)) return -1;
  const value = Number(raw);
  return Number.isSafeInteger(value) ? value : -1;
}

function parseContentRange(raw: string | null): ByteRange | undefined {
  if (raw === null || raw === "") return undefined;

  const invalid: ByteRange = { start: -1, end: -1, total: -1, raw };
  const match = /^bytes\s+(\d+)-(\d+)\/(\d+|\*)$/i.exec(raw);
  if (!match) return invalid;

  const start = parseHeaderInteger(match[1]);
  const end = parseHeaderInteger(match[2]);
  const total = match[3] === "*" ? -1 : parseHeaderInteger(match[3]);
  if (start < 0 || end < start || (total !== -1 && total <= end)) {
    return invalid;
  }
  return { start, end, total, raw };
}

export function createReadBytesResponse<TBody>(
  response: Response,
  body: TBody,
): ReadBytesResponse<TBody> {
  const isPartial = response.status === 206;
  const contentRange = isPartial
    ? parseContentRange(response.headers.get("content-range"))
    : undefined;
  const contentLength = parseHeaderInteger(response.headers.get("content-length"));

  return {
    body,
    statusCode: response.status,
    contentType: response.headers.get("content-type") ?? undefined,
    contentDisposition: response.headers.get("content-disposition") ?? undefined,
    contentLength,
    totalSize: isPartial ? (contentRange?.total ?? -1) : contentLength,
    contentRange,
    isPartial,
  };
}

class ResponseByteStream implements ReadBytesStream {
  private reader?: ReadableStreamDefaultReader<Uint8Array>;
  private started = false;
  private closed = false;

  constructor(private readonly response: Response) {}

  async close(): Promise<void> {
    if (this.closed) return;
    this.closed = true;

    if (this.reader) {
      await this.reader.cancel().catch(() => undefined);
      this.reader.releaseLock();
      this.reader = undefined;
      return;
    }
    await this.response.body?.cancel().catch(() => undefined);
  }

  async *[Symbol.asyncIterator](): AsyncIterator<Uint8Array> {
    if (this.started) {
      throw new Error("Download body can only be read once");
    }
    this.started = true;
    if (this.closed) return;

    const body = this.response.body as ReadableStream<Uint8Array> | null;
    if (!body) {
      this.closed = true;
      return;
    }

    this.reader = body.getReader();
    try {
      while (true) {
        const { done, value } = await this.reader.read();
        if (done) return;
        if (value) yield value;
      }
    } finally {
      await this.close();
    }
  }
}

export function readResponseBody(response: Response): ReadBytesStream {
  return new ResponseByteStream(response);
}
