// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

import { SandboxApiException, SandboxException } from '@alibaba-group/opensandbox';

export type BindingErrorCode = 'invalid_options' | 'aborted' | 'binding_open_failed' |
  'cleanup_unknown' | 'close_failed' | 'outcome_unknown' | 'binding_closed';

/** Safe, structured errors deliberately omit SDK causes, headers, URLs and request bodies. */
export class BindingError extends Error {
  constructor(readonly code: BindingErrorCode, message: string) {
    super(message);
    this.name = 'BindingError';
  }
}

export type OpenStage = 'connect' | 'readiness' | 'preflight';
export type CleanupState = 'killed' | 'cleanup-unknown' | 'not-owned';

export class BindingOpenError extends BindingError {
  constructor(readonly stage: OpenStage, readonly sandboxId: string, readonly cleanupState: CleanupState) {
    super('binding_open_failed', `Sandbox binding failed during ${stage}; cleanup: ${cleanupState}.`);
    this.name = 'BindingOpenError';
  }
}

export class CleanupUnknownError extends BindingError {
  constructor(readonly sandboxId: string) {
    super('cleanup_unknown', 'Sandbox deletion is unconfirmed. An explicit retry is allowed.');
    this.name = 'CleanupUnknownError';
  }
}

/** Unknown outcomes must not be retried automatically, especially create and mutation POSTs. */
export class UnknownOutcomeError extends BindingError {
  declare readonly classification?: SdkTransportClassification;
  declare readonly statusCode?: number;
  declare readonly sdkCode?: SafeSdkErrorCode;

  constructor(
    readonly operation: 'create' | 'command' | 'mutation',
    readonly correlationId: string,
    readonly sandboxId?: string,
    failure?: unknown,
  ) {
    super('outcome_unknown', `Remote ${operation} outcome is unknown. Do not retry automatically.`);
    this.name = 'UnknownOutcomeError';
    if (failure !== undefined) Object.assign(this, safeSdkFailure(failure));
  }
}

/** Close is terminal: this error is local refusal, not evidence of a remote delete attempt. */
export class BindingClosedError extends BindingError {
  constructor(readonly sandboxId: string) {
    super('binding_closed', 'This binding is closed. Connect explicitly to the saved sandbox ID with fresh connection configuration before deletion or retry.');
    this.name = 'BindingClosedError';
  }
}

export type SdkTransportOperation = 'run' | 'runStream' | 'interrupt' | 'getCommandStatus' |
  'writeBytes' | 'readBytes' | 'readBytesStream' | 'createDirectories' | 'deleteFiles' | 'deleteDirectories';
export type SdkTransportClassification = 'api' | 'aborted' | 'invalid-argument' | 'ready-timeout' | 'unhealthy' | 'transport';
export type SafeSdkErrorCode = 'INTERNAL_UNKNOWN_ERROR' | 'READY_TIMEOUT' | 'UNHEALTHY' | 'INVALID_ARGUMENT' | 'UNEXPECTED_RESPONSE' | 'FSB::API_ERROR';
const SDK_CODES: readonly string[] = ['INTERNAL_UNKNOWN_ERROR', 'READY_TIMEOUT', 'UNHEALTHY', 'INVALID_ARGUMENT', 'UNEXPECTED_RESPONSE', 'FSB::API_ERROR'];

/** Copy only bounded, allowlisted facts; never retain the raw failure. */
function safeSdkFailure(failure: unknown): {
  classification: SdkTransportClassification; statusCode?: number; sdkCode?: SafeSdkErrorCode;
} {
  const result: { classification: SdkTransportClassification; statusCode?: number; sdkCode?: SafeSdkErrorCode } = { classification: 'transport' };
  const sdkCode = failure instanceof SandboxException ? failure.error?.code : undefined;
  if (typeof sdkCode === 'string' && SDK_CODES.includes(sdkCode)) result.sdkCode = sdkCode as SafeSdkErrorCode;
  if (failure instanceof SandboxApiException) {
    result.classification = 'api';
    const status = failure.statusCode;
    if (typeof status === 'number' && Number.isInteger(status) && status >= 100 && status <= 599) result.statusCode = status;
  } else if (failure instanceof Error && failure.name === 'AbortError') {
    result.classification = 'aborted';
  } else if (result.sdkCode === 'INVALID_ARGUMENT') {
    result.classification = 'invalid-argument';
  } else if (result.sdkCode === 'READY_TIMEOUT') {
    result.classification = 'ready-timeout';
  } else if (result.sdkCode === 'UNHEALTHY') {
    result.classification = 'unhealthy';
  }
  return result;
}

/** Sanitized public boundary. Never retain a raw SDK error, diagnostic message or request ID. */
export class SdkTransportError extends Error {
  readonly code = 'sdk_transport_failed';
  declare readonly classification: SdkTransportClassification;
  declare readonly statusCode?: number;
  declare readonly sdkCode?: SafeSdkErrorCode;

  constructor(readonly operation: SdkTransportOperation, failure: unknown) {
    super(`OpenSandbox SDK ${operation} failed.`);
    this.name = 'SdkTransportError';
    Object.assign(this, safeSdkFailure(failure));
  }
}
