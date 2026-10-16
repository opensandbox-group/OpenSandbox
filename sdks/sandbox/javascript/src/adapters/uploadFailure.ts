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

import { isOpaqueRedirect } from "../config/redirectFetch.js";
import { SandboxApiException, SandboxError } from "../core/exceptions.js";

/**
 * Surface a failed (non-2xx) upload response as a `SandboxApiException`.
 *
 * Uploads never follow redirects, so a 3xx reaches the adapter directly. In
 * browsers `redirect: "manual"` yields an opaque response (status 0 with an
 * unreadable body and no `Location`), so report that clearly instead of the
 * misleading `status=0`.
 */
export async function throwOnUploadFailure(response: Response): Promise<void> {
  if (response.ok) return;

  if (isOpaqueRedirect(response)) {
    throw new SandboxApiException({
      message:
        "Upload failed: the redirect response cannot be inspected in this runtime",
      statusCode: response.status,
      error: new SandboxError(SandboxError.UNEXPECTED_RESPONSE, "Upload failed"),
    });
  }

  const requestId = response.headers.get("x-request-id") ?? undefined;
  const rawBody = await response.text().catch(() => undefined);
  throw new SandboxApiException({
    message: `Upload failed (status=${response.status})`,
    statusCode: response.status,
    requestId,
    error: new SandboxError(SandboxError.UNEXPECTED_RESPONSE, "Upload failed"),
    rawBody,
  });
}
