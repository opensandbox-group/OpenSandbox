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

import { SandboxApiException, SandboxError } from "../core/exceptions.js";

function tryParseJson(line: string): unknown | undefined {
  try {
    return JSON.parse(line);
  } catch {
    return undefined;
  }
}

/**
 * Parses an SSE-like stream that may be either:
 * - standard SSE frames (`data: {...}\n\n`)
 * - newline-delimited JSON (one JSON object per line)
 */
export async function* parseJsonEventStream<T>(
  res: Response,
  opts?: { fallbackErrorMessage?: string },
): AsyncIterable<T> {
  if (!res.ok) {
    const text = await res.text().catch(() => "");
    const parsed = tryParseJson(text);
    const err = parsed && typeof parsed === "object" ? (parsed as any) : undefined;
    const requestId = res.headers.get("x-request-id") ?? undefined;
    const message = err?.message ?? opts?.fallbackErrorMessage ?? `Stream request failed (status=${res.status})`;
    const code = err?.code ? String(err.code) : SandboxError.UNEXPECTED_RESPONSE;
    throw new SandboxApiException({
      message,
      statusCode: res.status,
      requestId,
      error: new SandboxError(code, err?.message ? String(err.message) : message),
      rawBody: parsed ?? text,
    });
  }

  if (!res.body) {
    return;
  }

  const reader = res.body.getReader();
  const decoder = new TextDecoder("utf-8");
  let buf = "";

  try {
    while (true) {
      const { value, done } = await reader.read();
      if (done) break;

      buf += decoder.decode(value, { stream: true });
      let idx: number;

      while ((idx = buf.indexOf("\n")) >= 0) {
        const rawLine = buf.slice(0, idx);
        buf = buf.slice(idx + 1);

        const line = rawLine.trim();
        if (!line) continue;

        // Support standard SSE "data:" prefix
        if (line.startsWith(":")) continue;
        if (line.startsWith("event:") || line.startsWith("id:") || line.startsWith("retry:")) continue;

        const jsonLine = line.startsWith("data:") ? line.slice("data:".length).trim() : line;
        if (!jsonLine) continue;

        const parsed = tryParseJson(jsonLine);
        if (!parsed) continue;
        yield parsed as T;
      }
    }

    // Flush any buffered UTF-8 bytes from the decoder.
    buf += decoder.decode();

    // flush last line if exists
    const last = buf.trim();
    if (last) {
      const jsonLine = last.startsWith("data:") ? last.slice("data:".length).trim() : last;
      const parsed = tryParseJson(jsonLine);
      if (parsed) yield parsed as T;
    }
  } finally {
    // Consumers (e.g. background commands) can stop iterating once
    // execution_complete arrives, without draining the rest of the
    // stream. Cancel the reader on any exit path — normal completion,
    // early break, or thrown error — so the fetch body is released
    // instead of staying locked and holding the transport connection
    // open (#1528, #1532).
    await reader.cancel().catch(() => undefined);
  }
}