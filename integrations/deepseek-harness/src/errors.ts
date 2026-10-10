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
  constructor(
    readonly operation: 'create' | 'command' | 'mutation',
    readonly correlationId: string,
    readonly sandboxId?: string,
  ) {
    super('outcome_unknown', `Remote ${operation} outcome is unknown. Do not retry automatically.`);
    this.name = 'UnknownOutcomeError';
  }
}

/** Close is terminal: this error is local refusal, not evidence of a remote delete attempt. */
export class BindingClosedError extends BindingError {
  constructor(readonly sandboxId: string) {
    super('binding_closed', 'This SDK handle is closed. Connect explicitly to the saved sandbox ID with fresh connection configuration before deletion or retry.');
    this.name = 'BindingClosedError';
  }
}

export type SdkTransportOperation = 'run' | 'runStream' | 'interrupt' | 'getCommandStatus' |
  'writeBytes' | 'readBytes' | 'readBytesStream' | 'createDirectories' | 'deleteFiles' | 'deleteDirectories';
export type SdkTransportClassification = 'api' | 'aborted' | 'invalid-argument' | 'ready-timeout' | 'unhealthy' | 'transport';
export type SafeSdkErrorCode = 'INTERNAL_UNKNOWN_ERROR' | 'READY_TIMEOUT' | 'UNHEALTHY' | 'INVALID_ARGUMENT' | 'UNEXPECTED_RESPONSE';
const SDK_CODES: readonly string[] = ['INTERNAL_UNKNOWN_ERROR', 'READY_TIMEOUT', 'UNHEALTHY', 'INVALID_ARGUMENT', 'UNEXPECTED_RESPONSE'];

/** Sanitized public boundary. Never retain a raw SDK error, diagnostic message or request ID. */
export class SdkTransportError extends Error {
  readonly code = 'sdk_transport_failed';
  readonly classification: SdkTransportClassification;
  declare readonly statusCode?: number;
  declare readonly sdkCode?: SafeSdkErrorCode;

  constructor(readonly operation: SdkTransportOperation, failure: unknown) {
    super(`OpenSandbox SDK ${operation} failed.`);
    this.name = 'SdkTransportError';
    const sdkCode = failure instanceof SandboxException ? failure.error?.code : undefined;
    if (typeof sdkCode === 'string' && SDK_CODES.includes(sdkCode)) this.sdkCode = sdkCode as SafeSdkErrorCode;
    if (failure instanceof SandboxApiException) {
      this.classification = 'api';
      const status = failure.statusCode;
      if (typeof status === 'number' && Number.isInteger(status) && status >= 100 && status <= 599) this.statusCode = status;
    } else if (failure instanceof Error && failure.name === 'AbortError') {
      this.classification = 'aborted';
    } else if (this.sdkCode === 'INVALID_ARGUMENT') {
      this.classification = 'invalid-argument';
    } else if (this.sdkCode === 'READY_TIMEOUT') {
      this.classification = 'ready-timeout';
    } else if (this.sdkCode === 'UNHEALTHY') {
      this.classification = 'unhealthy';
    } else {
      this.classification = 'transport';
    }
  }
}
