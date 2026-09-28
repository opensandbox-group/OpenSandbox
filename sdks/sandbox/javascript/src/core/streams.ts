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

/**
 * Iterate a response body stream chunk by chunk.
 *
 * Shared by the filesystem adapters' `downloadStream`: the reader lock is
 * always released — cancel + releaseLock on early consumer exit or error
 * (#1528/#1532), plain releaseLock after a full read.
 */
export async function* iterateBodyStream(
  body: ReadableStream<Uint8Array>,
): AsyncGenerator<Uint8Array> {
  const reader = body.getReader();
  try {
    while (true) {
      const { done, value } = await reader.read();
      if (done) return;
      if (value) yield value;
    }
  } finally {
    // Release the body lock on early exit or error (#1528/#1532).
    await reader.cancel().catch(() => undefined);
    reader.releaseLock();
  }
}
