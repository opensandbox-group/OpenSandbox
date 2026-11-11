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

import type { RenewalPolicy } from "../models/sandboxes.js";

const EXTEND_SECONDS_MIN = 300;
const EXTEND_SECONDS_MAX = 86400;

/**
 * Validate an OSEP-0009 renewal policy client-side, mirroring the server's
 * create-time rules: `extendSeconds` must be an integer within 300-86400.
 */
export function validateRenewalPolicy(
  policy: RenewalPolicy,
  path = "lifecycle.renewalPolicy"
): void {
  const t = policy.extendSeconds;
  if (typeof t !== "number" || !Number.isInteger(t) || t < EXTEND_SECONDS_MIN || t > EXTEND_SECONDS_MAX) {
    throw new Error(
      `${path}.extendSeconds must be an integer between ${EXTEND_SECONDS_MIN} and ${EXTEND_SECONDS_MAX}, got ${String(t)}`
    );
  }
}
