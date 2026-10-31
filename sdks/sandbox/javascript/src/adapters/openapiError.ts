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

import { SandboxApiException, SandboxError } from "../core/exceptions.js";

export function throwOnOpenApiFetchError(
  result: { error?: unknown; response: Response },
  fallbackMessage: string,
): void {
  const requestId = result.response.headers.get("x-request-id") ?? undefined;
  const status = (result.response as any).status ?? 0;

  // `parseAs: "text"` surfaces error bodies as raw strings ("" when the body
  // is empty). Recover structured JSON payloads so error codes and messages
  // survive, and normalize empty bodies to undefined.
  let err: any = result.error;
  if (typeof err === "string") {
    if (!err.trim()) {
      err = undefined;
    } else {
      try {
        const parsed = JSON.parse(err);
        if (parsed && typeof parsed === "object") {
          err = parsed;
        }
      } catch {
        err = result.error;
      }
    }
  }

  // A non-2xx response is always an error, even without a parsable body
  // (execd identity routes reply with an empty 501/503). A failed request
  // must never look successful.
  const ok = (result.response as any).ok ?? (status >= 200 && status < 300);
  if (!err && ok !== false) return;

  let rawFragment: string | undefined;
  if (typeof err === "string") {
    rawFragment = err;
  } else if (err && typeof err === "object") {
    try {
      rawFragment = JSON.stringify(err);
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

/**
 * Throws the failed response of a direct `fetch` call as a
 * `SandboxApiException`, reading its body the same way as
 * {@link throwOnOpenApiFetchError}. It also throws for a 2xx response, so
 * callers that expect one specific status can pass any other.
 */
export async function throwOnFailedResponse(
  response: Response,
  fallbackMessage: string,
): Promise<never> {
  const body = await response.text().catch(() => "");
  throwOnOpenApiFetchError({ error: body, response }, fallbackMessage);
  // Only an unexpected 2xx status with an empty body gets here.
  throw new SandboxApiException({
    message: fallbackMessage,
    statusCode: response.status,
    requestId: response.headers.get("x-request-id") ?? undefined,
  });
}
