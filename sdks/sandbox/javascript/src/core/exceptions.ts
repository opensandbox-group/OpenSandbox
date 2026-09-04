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

export type SandboxErrorCode =
  | "INTERNAL_UNKNOWN_ERROR"
  | "READY_TIMEOUT"
  | "UNHEALTHY"
  | "INVALID_ARGUMENT"
  | "UNEXPECTED_RESPONSE"
  | "POOL_EMPTY"
  | "POOL_ACQUIRE_FAILED"
  | "POOL_NOT_RUNNING"
  | "POOL_STATE_STORE_UNAVAILABLE"
  // Allow server-defined codes as well.
  | (string & {});

/**
 * Structured error payload carried by {@link SandboxException}.
 *
 * - `code`: stable programmatic identifier
 * - `message`: optional human-readable message
 */
export class SandboxError {
  static readonly INTERNAL_UNKNOWN_ERROR: SandboxErrorCode = "INTERNAL_UNKNOWN_ERROR";
  static readonly READY_TIMEOUT: SandboxErrorCode = "READY_TIMEOUT";
  static readonly UNHEALTHY: SandboxErrorCode = "UNHEALTHY";
  static readonly INVALID_ARGUMENT: SandboxErrorCode = "INVALID_ARGUMENT";
  static readonly UNEXPECTED_RESPONSE: SandboxErrorCode = "UNEXPECTED_RESPONSE";
  static readonly POOL_EMPTY: SandboxErrorCode = "POOL_EMPTY";
  static readonly POOL_ACQUIRE_FAILED: SandboxErrorCode = "POOL_ACQUIRE_FAILED";
  static readonly POOL_NOT_RUNNING: SandboxErrorCode = "POOL_NOT_RUNNING";
  static readonly POOL_STATE_STORE_UNAVAILABLE: SandboxErrorCode = "POOL_STATE_STORE_UNAVAILABLE";

  constructor(
    readonly code: SandboxErrorCode,
    readonly message?: string,
  ) {}
}

interface SandboxExceptionOpts {
  message?: string;
  cause?: unknown;
  error?: SandboxError;
  requestId?: string;
}

/**
 * Base exception class for all SDK errors.
 *
 * All errors thrown by this SDK are subclasses of {@link SandboxException}.
 */
export class SandboxException extends Error {
  readonly name: string = "SandboxException";
  readonly error: SandboxError;
  readonly cause?: unknown;
  readonly requestId?: string;

  constructor(opts: SandboxExceptionOpts = {}) {
    super(opts.message);
    this.cause = opts.cause;
    this.error = opts.error ?? new SandboxError(SandboxError.INTERNAL_UNKNOWN_ERROR);
    this.requestId = opts.requestId;
  }
}

export class SandboxApiException extends SandboxException {
  readonly name: string = "SandboxApiException";
  readonly statusCode?: number;
  readonly rawBody?: unknown;

  constructor(opts: SandboxExceptionOpts & {
    statusCode?: number;
    rawBody?: unknown;
  }) {
    super({
      message: opts.message,
      cause: opts.cause,
      error: opts.error ?? new SandboxError(SandboxError.UNEXPECTED_RESPONSE, opts.message),
      requestId: opts.requestId,
    });
    this.statusCode = opts.statusCode;
    this.rawBody = opts.rawBody;
  }
}

export class SandboxInternalException extends SandboxException {
  readonly name: string = "SandboxInternalException";

  constructor(opts: { message?: string; cause?: unknown }) {
    super({
      message: opts.message,
      cause: opts.cause,
      error: new SandboxError(SandboxError.INTERNAL_UNKNOWN_ERROR, opts.message),
    });
  }
}

export class SandboxUnhealthyException extends SandboxException {
  readonly name: string = "SandboxUnhealthyException";

  constructor(opts: { message?: string; cause?: unknown }) {
    super({
      message: opts.message,
      cause: opts.cause,
      error: new SandboxError(SandboxError.UNHEALTHY, opts.message),
    });
  }
}

export class SandboxReadyTimeoutException extends SandboxException {
  readonly name: string = "SandboxReadyTimeoutException";

  constructor(opts: { message?: string; cause?: unknown }) {
    super({
      message: opts.message,
      cause: opts.cause,
      error: new SandboxError(SandboxError.READY_TIMEOUT, opts.message),
    });
  }
}

export class InvalidArgumentException extends SandboxException {
  readonly name: string = "InvalidArgumentException";

  constructor(opts: { message?: string; cause?: unknown }) {
    super({
      message: opts.message,
      cause: opts.cause,
      error: new SandboxError(SandboxError.INVALID_ARGUMENT, opts.message),
    });
  }
}

export class PoolEmptyException extends SandboxException {
  readonly name: string = "PoolEmptyException";

  constructor(poolName: string) {
    const message = `Sandbox pool '${poolName}' has no idle sandbox available`;
    super({ message, error: new SandboxError(SandboxError.POOL_EMPTY, message) });
  }
}

export class PoolAcquireFailedException extends SandboxException {
  readonly name: string = "PoolAcquireFailedException";

  constructor(poolName: string, cause?: unknown) {
    const message = `Sandbox pool '${poolName}' could not acquire a usable idle sandbox`;
    super({ message, cause, error: new SandboxError(SandboxError.POOL_ACQUIRE_FAILED, message) });
  }
}

export class PoolNotRunningException extends SandboxException {
  readonly name: string = "PoolNotRunningException";

  constructor(poolName: string, state: string) {
    const message = `Sandbox pool '${poolName}' is not running (state=${state})`;
    super({ message, error: new SandboxError(SandboxError.POOL_NOT_RUNNING, message) });
  }
}

export class PoolStateStoreUnavailableException extends SandboxException {
  readonly name: string = "PoolStateStoreUnavailableException";

  constructor(operation: string, cause?: unknown) {
    const message = `Sandbox pool state store operation '${operation}' failed`;
    super({
      message,
      cause,
      error: new SandboxError(SandboxError.POOL_STATE_STORE_UNAVAILABLE, message),
    });
  }
}
