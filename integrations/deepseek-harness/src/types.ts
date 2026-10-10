// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

import type {
  ConnectionConfig, ConnectionConfigOptions, Sandbox, SandboxCreateOptions,
  SandboxCreateFromTemplateOptions, SandboxConnectOptions,
} from '@alibaba-group/opensandbox';

/** The only persisted binding data. Connection and model credentials stay separate. */
export interface BindingDescriptor {
  readonly version: 1;
  readonly sessionId: string;
  readonly sandboxId: string;
  readonly remoteCwd: string;
}

export type BindingState = 'open' | 'closed' | 'killed' | 'cleanup-unknown';

export interface BoundSandbox {
  readonly descriptor: BindingDescriptor;
  readonly sandbox: Sandbox;
  readonly state: BindingState;
  /**
   * Terminally close this SDK handle; never delete the remote sandbox.
   * A reused transport-initialized ConnectionConfig shares its dispatcher: closing
   * this handle also closes that dispatcher for other bindings using it.
   */
  close(): Promise<void>;
  /** Delete while this handle is open; explicit retries are allowed until close. After close, reconnect explicitly with fresh connection configuration. */
  kill(): Promise<void>;
  toJSON(): BindingDescriptor;
}

type ManagedSdkOptions = 'connectionConfig' | 'adapterFactory' | 'skipHealthCheck' |
  'healthCheck' | 'readyTimeoutSeconds' | 'healthCheckPollingInterval' | 'signal';

interface BindingOptions {
  sessionId: string;
  /** Absolute Linux sandbox path. It is never mapped to a host path. */
  remoteCwd: string;
  /**
   * SDK 1.1.0 gives plain options and fresh, uninitialized instances independent transports.
   * Reusing a transport-initialized instance (including sandbox.connectionConfig) shares
   * its dispatcher; any binding close, including failed opening cleanup, closes it for all.
   * Prefer plain options or independently initialized instances per binding, and fresh
   * configuration after close. Caller fields are not changed or frozen by this integration.
   */
  connectionConfig: ConnectionConfig | ConnectionConfigOptions;
  signal?: AbortSignal;
  readyTimeoutSeconds?: number;
  healthCheckPollingInterval?: number;
  /** Separate application-prerequisite budget, independent of SDK readiness. */
  preflightTimeoutSeconds?: number;
}

export type OpenOptions = BindingOptions & (
  | ({ kind: 'create-image'; image: NonNullable<SandboxCreateOptions['image']> } &
      Omit<SandboxCreateOptions, ManagedSdkOptions | 'image' | 'snapshotId'>)
  | ({ kind: 'create-template' } & Omit<SandboxCreateFromTemplateOptions, ManagedSdkOptions>)
  | ({ kind: 'connect' } & Omit<SandboxConnectOptions, ManagedSdkOptions>)
);

/** Explicit injection boundary. Its signatures are the published SDK 1.1.0 facade. */
export type SdkFacade = Pick<typeof Sandbox, 'create' | 'createFromTemplate' | 'connect'>;
