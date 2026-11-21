// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

export { openBinding, createBindingOpener } from './binding.js';
export type { BindingDescriptor, BindingState, BoundSandbox, OpenOptions, SdkFacade } from './types.js';
export {
  BindingError, BindingOpenError, BindingClosedError, CleanupUnknownError, UnknownOutcomeError, SdkTransportError,
} from './errors.js';
export type {
  BindingErrorCode, OpenStage, CleanupState, SdkTransportOperation, SdkTransportClassification, SafeSdkErrorCode,
} from './errors.js';
export { SdkTransport } from './sdk-transport.js';
export type { ByteReadOptions, ByteWriteOptions, DirectoryEntry } from './sdk-transport.js';
export { RemoteShell, RemoteShellError } from './shell.js';
export type { RemoteShellConfig, RemoteShellOptions, ShellErrorCode } from './shell.js';
export { RemoteFileSystem } from './filesystem.js';
export type { RemoteFileSystemConfig } from './filesystem.js';
export { RemoteFsError } from './helper-client.js';
export { createHeadlessSession } from './headless.js';
export type { HeadlessSession, HeadlessSessionOptions } from './headless.js';
