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
   * Terminally close this binding; never delete the remote sandbox.
   * A reused transport-initialized ConnectionConfig shares its dispatcher but remains
   * caller-owned by default. Only SDK-owned transports are closed; connect can opt into
   * closing shared transport with ownsTransport: true, affecting every sharer.
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
   * SDK 1.1.1 gives plain options and fresh, uninitialized instances independent transports.
   * A transport-initialized instance shares its dispatcher but stays caller-owned by default:
   * binding close and failed opening cleanup leave it open. The owner must close it when done.
   * Connect's ownsTransport: true opts into closing it; closing that binding or any sandbox
   * that owns the same transport invalidates every sharer. Prefer plain options or
   * independent configurations, and fresh configuration after the owner closes the transport.
   * Caller fields are not changed or frozen by this integration.
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

/** Explicit injection boundary. Its signatures are the published SDK 1.1.1 facade. */
export type SdkFacade = Pick<typeof Sandbox, 'create' | 'createFromTemplate' | 'connect'>;
