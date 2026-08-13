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

export function throwOnOpenApiFetchError(
  result: { error?: unknown; response: Response },
  fallbackMessage: string,
): void {
  if (!result.error) return;

  const requestId = result.response.headers.get("x-request-id") ?? undefined;
  const status = (result.response as any).status ?? 0;

  const err = result.error as any;

  let rawFragment: string | undefined;
  if (typeof result.error === "string") {
    rawFragment = result.error;
  } else if (result.error && typeof result.error === "object") {
    try {
      rawFragment = JSON.stringify(result.error);
    } catch {
      rawFragment = undefined;
    }
  }

  const message =
    err?.message ??
    err?.error?.message ??
    (rawFragment && rawFragment.length > 0 ? rawFragment : fallbackMessage);

  const code = err?.code ?? err?.error?.code;
  const msg = err?.message ?? err?.error?.message ?? message;

  throw new SandboxApiException({
    message: msg,
    statusCode: status,
    requestId,
    error: code ? new SandboxError(String(code), String(msg ?? "")) : new SandboxError(SandboxError.UNEXPECTED_RESPONSE, String(msg ?? "")),
    rawBody: result.error,
  });
}