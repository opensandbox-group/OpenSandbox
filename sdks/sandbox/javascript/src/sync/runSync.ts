// Copyright 2025 Alibaba Group Holding Ltd.
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
 * Synchronous execution utility for Node.js.
 *
 * Uses `worker_threads` + `Atomics.wait` to block the calling thread until
 * an async task completes in a child worker thread. This is the standard
 * Node.js technique for exposing a synchronous API over async operations.
 *
 * IMPORTANT: This module only works in Node.js. It will throw in environments
 * that do not support `worker_threads` (e.g. browsers, Deno).
 */

import type {
  CreatePoolRequest,
  PoolInfo,
  PoolListResponse,
  UpdatePoolRequest,
} from "../models/pools.js";
import type { PoolManagerOptions } from "../poolManager.js";

// ---------------------------------------------------------------------------
// Worker payload types  (serialised through the SharedArrayBuffer channel)
// ---------------------------------------------------------------------------

/** The set of pool operations the worker can execute. */
export type PoolOp =
  | { op: "createPool"; req: CreatePoolRequest }
  | { op: "getPool"; poolName: string }
  | { op: "listPools" }
  | { op: "updatePool"; poolName: string; req: UpdatePoolRequest }
  | { op: "deletePool"; poolName: string }
  | { op: "close" };

/** Message sent from main → worker. */
export interface WorkerRequest {
  sharedBuffer: SharedArrayBuffer;
  managerOptions: PoolManagerOptions;
  payload: PoolOp;
}

/** Message sent back from worker → main (written into sharedBuffer). */
export interface WorkerResult {
  ok: boolean;
  /** Serialised return value (JSON). */
  value?: string;
  /** Serialised error info. */
  errorMessage?: string;
  errorName?: string;
  errorStatusCode?: number;
}

// ---------------------------------------------------------------------------
// Worker entry-point script (inlined as a string so no extra file is needed)
// ---------------------------------------------------------------------------

/**
 * Source of the worker script (CommonJS-compatible, used with `vm` + eval
 * to avoid needing a separate worker file that might not be findable at
 * runtime after bundling).
 */
export const WORKER_SCRIPT = /* js */ `
const { workerData, parentPort } = require("worker_threads");

// The worker receives the request via workerData (not a message) so it can
// start immediately without an async message round-trip.
const { sharedBuffer, managerOptions, payload } = workerData;

async function main() {
  // Dynamically import the PoolManager. We resolve from the worker's own
  // __filename so relative imports work whether run from src/ or dist/.
  // The caller sets workerData.__sdkModulePath to the absolute path of
  // poolManager.js so we don't have to guess.
  const { PoolManager } = await import(workerData.__sdkModulePath);

  const manager = PoolManager.create(managerOptions);
  let result;

  try {
    switch (payload.op) {
      case "createPool":
        result = await manager.createPool(payload.req);
        break;
      case "getPool":
        result = await manager.getPool(payload.poolName);
        break;
      case "listPools":
        result = await manager.listPools();
        break;
      case "updatePool":
        result = await manager.updatePool(payload.poolName, payload.req);
        break;
      case "deletePool":
        await manager.deletePool(payload.poolName);
        result = undefined;
        break;
      case "close":
        await manager.close();
        result = undefined;
        break;
      default:
        throw new Error("Unknown op: " + payload.op);
    }
  } finally {
    if (payload.op !== "close") {
      await manager.close().catch(() => {});
    }
  }

  // Serialise success result.
  const out = {
    ok: true,
    value: result !== undefined ? JSON.stringify(result) : undefined,
  };
  parentPort.postMessage(out);
}

main().catch((err) => {
  const out = {
    ok: false,
    errorMessage: err?.message ?? String(err),
    errorName: err?.name,
    errorStatusCode: err?.statusCode,
  };
  parentPort.postMessage(out);
});
`;

// ---------------------------------------------------------------------------
// Synchronous runner
// ---------------------------------------------------------------------------

/**
 * Run a Pool operation synchronously in a child worker thread.
 *
 * @param sdkModulePath - Absolute path to the compiled `poolManager.js` file.
 * @param managerOptions - Options forwarded to `PoolManager.create()` in the worker.
 * @param payload - The pool operation to execute.
 * @returns Deserialised result, or `undefined` for void operations.
 * @throws Re-throws any error thrown by the worker, reconstructing
 *   `SandboxApiException` when `errorStatusCode` is present.
 */
export function runPoolOpSync(
  sdkModulePath: string,
  managerOptions: PoolManagerOptions,
  payload: PoolOp
): unknown {
  // Lazily import Node.js built-ins so this file can still be imported in
  // non-Node environments without crashing at module load time.
  // eslint-disable-next-line @typescript-eslint/no-require-imports
  const { Worker, isMainThread } = require("worker_threads") as typeof import("worker_threads");
  // eslint-disable-next-line @typescript-eslint/no-require-imports
  const { Script, createContext } = require("vm") as typeof import("vm");

  if (!isMainThread) {
    throw new Error(
      "runPoolOpSync must be called from the main thread, not from inside a Worker."
    );
  }

  // Shared memory: [0] = status flag (0 = pending, 1 = done), [1] unused
  const sharedBuffer = new SharedArrayBuffer(4);
  const flag = new Int32Array(sharedBuffer);

  // Create a one-shot worker that runs the WORKER_SCRIPT inline via eval.
  // We pass __sdkModulePath so the worker can import PoolManager without
  // guessing the path.
  const worker = new Worker(
    // Node ≥18 supports `eval` option for inline scripts.
    WORKER_SCRIPT,
    {
      eval: true,
      workerData: {
        sharedBuffer,
        managerOptions,
        payload,
        __sdkModulePath: sdkModulePath,
      },
    }
  );

  let workerResult: WorkerResult | null = null;
  let workerError: unknown = null;

  worker.once("message", (msg: WorkerResult) => {
    workerResult = msg;
    // Signal the main thread to wake up.
    Atomics.store(flag, 0, 1);
    Atomics.notify(flag, 0);
  });

  worker.once("error", (err) => {
    workerError = err;
    Atomics.store(flag, 0, 1);
    Atomics.notify(flag, 0);
  });

  // Block the main thread until the worker posts a result.
  Atomics.wait(flag, 0, 0);

  // Make sure the worker is cleaned up even if we throw below.
  worker.terminate().catch(() => {});

  if (workerError) {
    throw workerError;
  }

  if (!workerResult) {
    throw new Error("runPoolOpSync: worker terminated without posting a result.");
  }

  const result = workerResult as WorkerResult;
  if (!result.ok) {
    // Reconstruct a typed error where possible.
    const msg = result.errorMessage ?? "Unknown worker error";
    if (result.errorStatusCode !== undefined) {
      // Lazy-require SandboxApiException to avoid circular deps at load time.
      // eslint-disable-next-line @typescript-eslint/no-require-imports
      const { SandboxApiException } = require("../core/exceptions.js") as typeof import("../core/exceptions.js");
      throw new SandboxApiException({ message: msg, statusCode: result.errorStatusCode });
    }
    const err = new Error(msg);
    if (result.errorName) err.name = result.errorName;
    throw err;
  }

  return result.value !== undefined ? JSON.parse(result.value) : undefined;
}
