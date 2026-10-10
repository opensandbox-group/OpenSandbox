// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

import { randomUUID } from 'node:crypto';
import { Sandbox } from '@alibaba-group/opensandbox';
import { BindingClosedError, BindingError, BindingOpenError, CleanupUnknownError, UnknownOutcomeError } from './errors.js';
import type { CleanupState, OpenStage } from './errors.js';
import type { BindingDescriptor, BindingState, BoundSandbox, OpenOptions, SdkFacade } from './types.js';
import { SdkTransport } from './sdk-transport.js';

// Reserved creation tag: FastPath persists metadata as DNS-label keys.
const CREATION_REQUEST_METADATA_KEY = 'dsh-binding-request';
const MAX_TIMER_MILLIS = 2_147_483_647;

const PRECHECK = [
  'set -eu',
  'test "$(uname -s)" = Linux',
  'command -v bash >/dev/null',
  'command -v python3 >/dev/null',
  // These are prerequisites for the bundled helper, not a helper protocol success claim.
  "python3 -c 'import os,sys,json,hashlib,fcntl,tempfile,stat; sys.exit(0 if sys.version_info >= (3, 8) and os.path.isdir(os.getcwd()) and os.access(os.getcwd(), os.R_OK | os.W_OK | os.X_OK) else 1)'",
].join('; ');

class ManagedBinding implements BoundSandbox {
  readonly #descriptor: BindingDescriptor;
  readonly #sandbox: Sandbox;
  #state: BindingState = 'open';
  #localClosed = false;
  #resourcesReleased = false;
  #closing: Promise<void> | undefined;
  #killing: Promise<void> | undefined;

  constructor(descriptor: BindingDescriptor, sandbox: Sandbox) {
    this.#descriptor = Object.freeze({ ...descriptor });
    // Shallow only: routing-critical SDK references are pinned; ConnectionConfig stays caller-owned.
    Object.freeze(sandbox);
    this.#sandbox = sandbox;
    Object.freeze(this);
  }

  get descriptor(): BindingDescriptor { return this.#descriptor; }
  get sandbox(): Sandbox { return this.#sandbox; }
  get state(): BindingState { return this.#state; }
  toJSON(): BindingDescriptor { return this.#descriptor; }

  async close(): Promise<void> {
    if (this.#resourcesReleased) return;
    if (this.#closing) return this.#closing;
    // Closing is terminal even if release fails or is still pending. Never reuse the dispatcher.
    this.#localClosed = true;
    if (this.#state === 'open') this.#state = 'closed';
    this.#closing = (async () => {
      try {
        // Let an already accepted deletion settle before closing its transport.
        if (this.#killing) await this.#killing.catch(() => undefined);
        await this.#sandbox.close();
        this.#resourcesReleased = true;
      } catch {
        throw new BindingError('close_failed', 'Local SDK resource release failed.');
      }
    })();
    try { await this.#closing; } finally { this.#closing = undefined; }
  }

  async kill(): Promise<void> {
    if (this.#state === 'killed') return;
    if (this.#killing) return this.#killing;
    if (this.#localClosed) throw new BindingClosedError(this.#descriptor.sandboxId);
    this.#killing = (async () => {
      try {
        await this.#sandbox.kill();
        this.#state = 'killed';
      } catch {
        this.#state = 'cleanup-unknown';
        throw new CleanupUnknownError(this.#descriptor.sandboxId);
      }
    })();
    try { await this.#killing; } finally { this.#killing = undefined; }
  }
}

function positive(value: number, name: string): number {
  if (!Number.isFinite(value) || value <= 0) throw new BindingError('invalid_options', `${name} must be positive and finite.`);
  return value;
}

function validate(options: OpenOptions): void {
  if (!options.sessionId || /[\x00-\x1f\x7f]/.test(options.sessionId)) {
    throw new BindingError('invalid_options', 'A nonempty session ID without control characters is required.');
  }
  if (!options.remoteCwd.startsWith('/') || /[\x00-\x1f\x7f]/.test(options.remoteCwd)) {
    throw new BindingError('invalid_options', 'remoteCwd must be an absolute Linux path without control characters.');
  }
  if (options.kind === 'create-image' && !(typeof options.image === 'string' ? options.image : options.image?.uri)) {
    throw new BindingError('invalid_options', 'An explicit image is required.');
  }
  if (options.kind === 'create-template' && (!options.templateId || !Number.isFinite(options.timeoutSeconds) || options.timeoutSeconds <= 0)) {
    throw new BindingError('invalid_options', 'A template ID and a positive finite template TTL are required.');
  }
  if (options.kind === 'connect' && !options.sandboxId) {
    throw new BindingError('invalid_options', 'An explicit live sandbox ID is required.');
  }
  if (!['create-image', 'create-template', 'connect'].includes(options.kind)) {
    throw new BindingError('invalid_options', 'Unsupported sandbox opening mode.');
  }
}

/** Independently bound local wait: SDK abort may stop observing at response headers. */
async function preflight(sandbox: Sandbox, cwd: string, timeoutSeconds: number, signal?: AbortSignal): Promise<void> {
  const abort = new AbortController();
  const deadline = setTimeout(() => abort.abort(), Math.ceil(timeoutSeconds * 1000));
  const onAbort = () => abort.abort();
  signal?.addEventListener('abort', onAbort, { once: true });
  if (signal?.aborted) abort.abort();
  let commandId: string | undefined;
  const interrupt = () => {
    if (commandId) void sandbox.commands.interrupt(commandId).catch(() => undefined);
  };
  abort.signal.addEventListener('abort', interrupt, { once: true });
  let rejectWait!: () => void;
  const interrupted = new Promise<never>((_, reject) => {
    rejectWait = () => reject(new BindingError('aborted', 'Remote prerequisite check did not complete within its allowed wait.'));
    abort.signal.addEventListener('abort', rejectWait, { once: true });
  });
  try {
    if (abort.signal.aborted) throw new BindingError('aborted', 'Remote prerequisite check was cancelled.');
    const execution = new SdkTransport(sandbox).run(['bash', '-c', `set -eu; cd -P -- "$1"; ${PRECHECK}`, 'opensandbox-dsh-preflight', cwd], {
      // Execd expands its cwd; establish the literal path with the same physical traversal as os.chdir.
      workingDirectory: '/', background: false, timeoutSeconds,
    }, {
      skipAccumulation: true,
      onInit: init => { commandId = init.id; if (abort.signal.aborted) interrupt(); },
    }, abort.signal);
    const result = await Promise.race([execution, interrupted]);
    if (!result.complete || result.error || result.exitCode !== 0) {
      throw new BindingError('binding_open_failed', 'Linux, Python 3.8+, bash, cwd or helper prerequisites failed.');
    }
  } finally {
    clearTimeout(deadline);
    signal?.removeEventListener('abort', onAbort);
    abort.signal.removeEventListener('abort', interrupt);
    abort.signal.removeEventListener('abort', rejectWait);
  }
}

/** Injection is explicit and uses only SDK facade signatures; no production test switch. */
export function createBindingOpener(sdk: SdkFacade): (options: OpenOptions) => Promise<BoundSandbox> {
  return async options => {
    validate(options);
    const readyTimeoutSeconds = positive(options.readyTimeoutSeconds ?? 30, 'readyTimeoutSeconds');
    const pollingIntervalMillis = positive(options.healthCheckPollingInterval ?? 200, 'healthCheckPollingInterval');
    const preflightTimeoutSeconds = positive(options.preflightTimeoutSeconds ?? 10, 'preflightTimeoutSeconds');
    if (Math.ceil(preflightTimeoutSeconds * 1000) > MAX_TIMER_MILLIS) {
      throw new BindingError('invalid_options', 'preflightTimeoutSeconds must not exceed 2147483.647 seconds.');
    }
    if (options.signal?.aborted) throw new BindingError('aborted', 'Sandbox opening was cancelled before launch.');
    const { kind, sessionId, remoteCwd, preflightTimeoutSeconds: _, ...sdkOptions } = options;
    const correlationId = randomUUID();
    let sandbox: Sandbox;
    try {
      // Obtain the owned ID before readiness so partial preparation can be cleaned up.
      if (kind === 'create-image') {
        const imageOptions = sdkOptions as Extract<OpenOptions, { kind: 'create-image' }>;
        sandbox = await sdk.create({ ...imageOptions, metadata: { ...imageOptions.metadata, [CREATION_REQUEST_METADATA_KEY]: correlationId }, skipHealthCheck: true });
      } else if (kind === 'create-template') {
        const templateOptions = sdkOptions as Extract<OpenOptions, { kind: 'create-template' }>;
        sandbox = await sdk.createFromTemplate({ ...templateOptions, metadata: { ...templateOptions.metadata, [CREATION_REQUEST_METADATA_KEY]: correlationId }, skipHealthCheck: true });
      } else {
        const connectOptions = sdkOptions as Extract<OpenOptions, { kind: 'connect' }>;
        sandbox = await sdk.connect({ ...connectOptions, skipHealthCheck: true });
      }
    } catch {
      if (kind === 'connect') throw new BindingOpenError('connect', (options as Extract<OpenOptions, { kind: 'connect' }>).sandboxId, 'not-owned');
      throw new UnknownOutcomeError('create', correlationId);
    }
    const binding = new ManagedBinding({ version: 1, sessionId, sandboxId: sandbox.id, remoteCwd }, sandbox);
    let stage: OpenStage = 'readiness';
    try {
      await sandbox.waitUntilReady({ readyTimeoutSeconds, pollingIntervalMillis, ...(options.signal ? { signal: options.signal } : {}) });
      stage = 'preflight';
      await preflight(sandbox, remoteCwd, preflightTimeoutSeconds, options.signal);
      return binding;
    } catch {
      let cleanupState: CleanupState = 'not-owned';
      if (kind !== 'connect') {
        try { await binding.kill(); cleanupState = 'killed'; } catch { cleanupState = 'cleanup-unknown'; }
      }
      // Local release is independent of remote deletion evidence.
      await binding.close().catch(() => undefined);
      throw new BindingOpenError(stage, sandbox.id, cleanupState);
    }
  };
}

const sdkFacade: SdkFacade = {
  create: options => Sandbox.create(options),
  createFromTemplate: options => Sandbox.createFromTemplate(options),
  connect: options => Sandbox.connect(options),
};

export const openBinding = createBindingOpener(sdkFacade);
