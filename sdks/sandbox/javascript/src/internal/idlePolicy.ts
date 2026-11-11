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

import type { ActivityProbe, IdlePolicy } from "../models/sandboxes.js";

const IDLE_TIMEOUT_MIN_SECONDS = 30;
const IDLE_TIMEOUT_MAX_SECONDS = 86400;
const PROBE_TIMEOUT_MIN_SECONDS = 1;
const PROBE_TIMEOUT_MAX_SECONDS = 10;
const PROBE_PATH_MAX_LENGTH = 512;

function isInteger(value: unknown): value is number {
  return typeof value === "number" && Number.isInteger(value);
}

/**
 * Validate an OSEP-0024 idle policy before it is sent to the server.
 *
 * Mirrors the server's create-time validation so invalid policies fail fast
 * client-side: `idleTimeoutSeconds` must be an integer within 30-86400 and is
 * always paired with `onIdle` ("pause" only); `activeProbe` is only relevant
 * with auto-pause, requires port and path, and takes a 1-10 second timeout.
 */
export function validateIdlePolicy(policy: IdlePolicy, path = "lifecycle.idlePolicy"): void {
  const hasTimeout = policy.idleTimeoutSeconds !== undefined;
  if (hasTimeout) {
    const t = policy.idleTimeoutSeconds;
    if (!isInteger(t) || t < IDLE_TIMEOUT_MIN_SECONDS || t > IDLE_TIMEOUT_MAX_SECONDS) {
      throw new Error(
        `${path}.idleTimeoutSeconds must be an integer between ${IDLE_TIMEOUT_MIN_SECONDS} and ${IDLE_TIMEOUT_MAX_SECONDS}, got ${String(t)}`
      );
    }
  }
  if (policy.onIdle !== undefined) {
    if (!hasTimeout) {
      throw new Error(
        `${path}.onIdle must be omitted when idleTimeoutSeconds is omitted (a policy without a timeout is wake-only)`
      );
    }
    if (policy.onIdle !== "pause") {
      throw new Error(`${path}.onIdle must be "pause"`);
    }
  } else if (hasTimeout) {
    throw new Error(`${path}.onIdle is required when idleTimeoutSeconds is set`);
  }
  if (policy.wakeOnAccess !== undefined && typeof policy.wakeOnAccess !== "boolean") {
    throw new Error(`${path}.wakeOnAccess must be a boolean`);
  }
  if (policy.activeProbe !== undefined) {
    if (!hasTimeout) {
      throw new Error(
        `${path}.activeProbe requires idleTimeoutSeconds; the probe is relevant only with auto-pause`
      );
    }
    validateActivityProbe(policy.activeProbe, `${path}.activeProbe`);
  }
}

export function validateActivityProbe(probe: ActivityProbe, path: string): void {
  if (!isInteger(probe.port) || probe.port < 1 || probe.port > 65535) {
    throw new Error(`${path}.port must be an integer between 1 and 65535, got ${String(probe.port)}`);
  }
  if (typeof probe.path !== "string" || probe.path.length === 0) {
    throw new Error(`${path}.path must be a non-empty absolute HTTP path`);
  }
  if (probe.path.length > PROBE_PATH_MAX_LENGTH) {
    throw new Error(`${path}.path must not exceed ${PROBE_PATH_MAX_LENGTH} characters`);
  }
  if (!probe.path.startsWith("/")) {
    throw new Error(`${path}.path must be an absolute HTTP path starting with '/'`);
  }
  // eslint-disable-next-line no-control-regex -- control characters are exactly what we reject
  if (/[\s\x00-\x1f\x7f]/.test(probe.path)) {
    throw new Error(`${path}.path must not contain whitespace or control characters`);
  }
  if (probe.timeoutSeconds !== undefined) {
    const t = probe.timeoutSeconds;
    if (!isInteger(t) || t < PROBE_TIMEOUT_MIN_SECONDS || t > PROBE_TIMEOUT_MAX_SECONDS) {
      throw new Error(
        `${path}.timeoutSeconds must be an integer between ${PROBE_TIMEOUT_MIN_SECONDS} and ${PROBE_TIMEOUT_MAX_SECONDS}, got ${String(t)}`
      );
    }
  }
}
