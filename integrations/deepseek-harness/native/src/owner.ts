// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

import { createHash } from 'node:crypto';
import { Context } from '@deepseek-ai/cordis';
import type { Agent } from '@deepseek-ai/dsh-agent';
import { validateNativeConfig } from './config.js';
import type { NativeConfig, ResolvedNativeConfig } from './config.js';
import { openBinding } from './core/binding.js';
import { BindingOpenError, UnknownOutcomeError } from './core/errors.js';
import type { OpenStage, SafeSdkErrorCode, SdkTransportClassification } from './core/errors.js';
import { RemoteShell } from './core/shell.js';
import { RemoteFileSystem } from './core/filesystem.js';
import type { BindingDescriptor, BoundSandbox, OpenOptions } from './core/types.js';

export interface NativeProviders {
  readonly binding: BoundSandbox;
  readonly shell: RemoteShell;
  readonly fs: RemoteFileSystem;
}
export interface NativeProviderWorld extends NativeProviders { dispose(): Promise<void>; }
export interface NativeOwnerDependencies {
  readonly openBinding?: (options: OpenOptions) => Promise<BoundSandbox>;
  /** Explicit offline/test boundary, never selected by production configuration. */
  readonly createProviders?: (agent: Agent, binding: BoundSandbox, config: Readonly<ResolvedNativeConfig>, assertActive: () => void) => Promise<NativeProviderWorld>;
}
export type NativeBindingState = 'unadmitted' | 'unopened' | 'opening' | 'open' | 'reconnect-required' |
  'outcome-unknown' | 'failed' | 'killed' | 'cleanup-unknown' | 'delete-pending' | 'drain-failed' | 'configuration-mismatch';
export interface NativeFailureFacts {
  readonly correlationId?: string;
  readonly classification?: SdkTransportClassification;
  readonly statusCode?: number;
  readonly sdkCode?: SafeSdkErrorCode;
  readonly stage?: OpenStage;
}
export interface NativeStatus extends NativeFailureFacts {
  readonly sessionId: string;
  readonly remoteCwd: string;
  readonly state: NativeBindingState;
  readonly sandboxId?: string;
  readonly destinationMatches: boolean;
  /** Local state is not a remote health/TTL query or proof of present remote existence. */
  readonly remoteExistence: 'unverified' | 'deleted';
  readonly operation?: 'delete' | 'reconnect';
  readonly cleanupState?: 'killed' | 'cleanup-unknown' | 'not-owned';
  readonly failure?: NativeOwnerErrorCode;
}
export type NativeOwnerErrorCode = 'owner_busy' | 'owner_closed' | 'inactive_route' | 'not_admitted' |
  'reconnect_required' | 'outcome_unknown' | 'binding_open_failed' | 'binding_unavailable' | 'binding_required' |
  'operation_pending' | 'aborted' | 'invalid_sandbox_id' | 'sandbox_id_mismatch' | 'reconnect_failed' |
  'cleanup_unknown' | 'drain_failed' | 'close_failed' | 'disposal_incomplete' | 'provider_failed' | 'configuration_mismatch' | 'deletion_unresolved';
const messages: Record<NativeOwnerErrorCode, string> = {
  owner_busy: 'Another OpenSandbox native owner is still active.', owner_closed: 'OpenSandbox native admission is closed.',
  inactive_route: 'This exact Agent route is no longer active.', not_admitted: 'Native session creation has not been admitted.',
  reconnect_required: 'This session requires an explicit sandbox reconnect; no sandbox was created.',
  outcome_unknown: 'Sandbox creation outcome is unknown. No automatic retry was attempted.',
  binding_open_failed: 'Sandbox preparation failed. Inspect the safe cleanup state before acting.',
  binding_unavailable: 'This sandbox binding is unavailable. No replacement was created.',
  binding_required: 'No known sandbox is available for deletion. No sandbox was created.',
  operation_pending: 'An explicit sandbox lifecycle operation is pending.', aborted: 'The caller stopped waiting.',
  invalid_sandbox_id: 'An explicit valid sandbox ID is required.', sandbox_id_mismatch: 'The confirmation does not match this session sandbox.',
  reconnect_failed: 'Sandbox reconnect failed. No sandbox was created.', cleanup_unknown: 'Sandbox deletion is unconfirmed. Retry only as a new explicit action.',
  drain_failed: 'Previous Agent provider draining is incomplete; this session cannot be reused.',
  close_failed: 'Local SDK resource release is incomplete.', disposal_incomplete: 'Native disposal did not fully settle within its allowed wait.',
  provider_failed: 'Remote provider preparation failed; no replacement sandbox was created.',
  deletion_unresolved: 'An earlier sandbox deletion is pending or unconfirmed. Reconnect and execution are blocked; verify remote cleanup externally before starting a new session.',
  configuration_mismatch: 'The saved session belongs to another OpenSandbox destination or working directory. Restore its configuration or start a new session.',
};
export class NativeOwnerError extends Error {
  constructor(readonly code: NativeOwnerErrorCode) { super(messages[code]); this.name = 'NativeOwnerError'; }
}
export interface NativeRoute {
  readonly sessionId: string;
  readonly remoteCwd: string;
  assertActive(): void;
  acquire(signal?: AbortSignal): Promise<NativeProviders>;
  peekProviders(): NativeProviders | undefined;
  status(): NativeStatus;
  reconnect(sandboxId: string, signal?: AbortSignal): Promise<NativeStatus>;
  delete(confirmSandboxId: string): Promise<NativeStatus>;
  dispose(): Promise<void>;
}

interface RecordEntry {
  readonly destination: string;
  diagnostic?: NativeFailureFacts;
  state: NativeBindingState;
  revision: number;
  descriptor?: BindingDescriptor;
  cleanupState?: NativeStatus['cleanupState'];
  failure?: NativeOwnerErrorCode;
}
interface RootLedger { readonly version: 1; readonly entries: Map<string, RecordEntry>; active?: object; }
// Store only safe facts and an inert ownership token on the root, never config, SDK handles or Agents.
// Symbol.for also preserves tombstones across a normal module/config remount on the same root.
const ledgerKey = Symbol.for('@opensandbox/deepseek-harness-native/ledger/v1');
interface Slot {
  readonly sessionId: string;
  readonly record: RecordEntry;
  binding?: BoundSandbox;
  opening?: Promise<BoundSandbox>;
  abort?: AbortController;
  route?: RouteState;
  previousDrain?: Promise<void>;
  generation: number;
  operation?: 'delete' | 'reconnect';
  mutation?: Promise<NativeStatus>;
  mutationId?: string;
}
interface RouteState {
  readonly agent: Agent;
  readonly slot: Slot;
  readonly generation: number;
  readonly fresh: boolean;
  disposed: boolean;
  providers?: NativeProviderWorld;
  preparing?: Promise<NativeProviderWorld>;
  creatingProviders?: boolean;
  draining?: Promise<void>;
  predecessor?: Promise<void>;
}
const error = (code: NativeOwnerErrorCode): NativeOwnerError => new NativeOwnerError(code);
const validId = (value: unknown): value is string => typeof value === 'string' && /^[A-Za-z0-9][A-Za-z0-9._-]{0,255}$/.test(value);

/** Copy only the existing core's bounded allowlist, never arbitrary Error properties or causes. */
function safeFailureFacts(failure: unknown): NativeFailureFacts {
  if (failure instanceof BindingOpenError) {
    return ['connect', 'readiness', 'preflight'].includes(failure.stage) ? Object.freeze({ stage: failure.stage }) : Object.freeze({});
  }
  if (!(failure instanceof UnknownOutcomeError)) return Object.freeze({});
  const classifications: readonly string[] = ['api', 'aborted', 'invalid-argument', 'ready-timeout', 'unhealthy', 'transport'];
  const sdkCodes: readonly string[] = ['INTERNAL_UNKNOWN_ERROR', 'READY_TIMEOUT', 'UNHEALTHY', 'INVALID_ARGUMENT', 'UNEXPECTED_RESPONSE', 'FSB::API_ERROR'];
  return Object.freeze({
    ...(validId(failure.correlationId) ? { correlationId: failure.correlationId } : {}),
    ...(failure.classification && classifications.includes(failure.classification) ? { classification: failure.classification } : {}),
    ...(typeof failure.statusCode === 'number' && Number.isInteger(failure.statusCode) && failure.statusCode >= 100 && failure.statusCode <= 599 ? { statusCode: failure.statusCode } : {}),
    ...(failure.sdkCode && sdkCodes.includes(failure.sdkCode) ? { sdkCode: failure.sdkCode } : {}),
  });
}


/** Cancellation belongs to this waiter, not the shared opening/provider operation. */
async function waitFor<T>(promise: Promise<T>, signal?: AbortSignal): Promise<T> {
  if (!signal) return promise;
  if (signal.aborted) throw error('aborted');
  let abort!: () => void;
  const cancelled = new Promise<never>((_, reject) => { abort = () => reject(error('aborted')); signal.addEventListener('abort', abort, { once: true }); });
  try { return await Promise.race([promise, cancelled]); }
  finally { signal.removeEventListener('abort', abort); }
}
async function bounded<T>(promise: Promise<T>, milliseconds: number, code: NativeOwnerErrorCode): Promise<T> {
  let timer: ReturnType<typeof setTimeout> | undefined;
  const timeout = new Promise<never>((_, reject) => { timer = setTimeout(() => reject(error(code)), milliseconds); });
  try { return await Promise.race([promise, timeout]); } finally { clearTimeout(timer); }
}

/** A private provider Context is not another Agent, model adapter or session. */
async function createProviderWorld(_agent: Agent, binding: BoundSandbox, config: Readonly<ResolvedNativeConfig>, assertActive: () => void): Promise<NativeProviderWorld> {
  const ctx = new Context(); let shell: RemoteShell | undefined, fs: RemoteFileSystem | undefined;
  let disposing: Promise<void> | undefined;
  const dispose = (): Promise<void> => disposing ??= (async () => {
    const results = await Promise.allSettled([Promise.resolve().then(() => shell?.dispose()), Promise.resolve().then(() => fs?.dispose())]);
    // Context teardown is independent of either provider's result.
    const context = await Promise.allSettled([Promise.resolve().then(() => ctx.fiber.dispose())]);
    if ([...results, ...context].some(result => result.status === 'rejected')) throw error('drain_failed');
  })();
  try {
    assertActive(); await ctx.plugin(RemoteShell, { binding, settlementTimeoutMs: config.settlementTimeoutMs });
    shell = ctx.shell as RemoteShell; assertActive();
    await ctx.plugin(RemoteFileSystem, { binding, settlementTimeoutMs: config.settlementTimeoutMs });
    fs = ctx.fs as RemoteFileSystem; assertActive();
    await fs.ready(); assertActive();
    return Object.freeze({ binding, shell, fs, dispose });
  } catch (failure) {
    try { await dispose(); } catch { throw error('drain_failed'); }
    throw failure instanceof NativeOwnerError ? failure : error('provider_failed');
  }
}

/** One plugin lifetime owns all live SDK handles; Agents own only provider worlds. */
export class NativeOwner {
  readonly #config: Readonly<ResolvedNativeConfig>;
  readonly #destination: string;
  readonly #isLive: (agent: Agent) => boolean;
  readonly #dependencies: Required<NativeOwnerDependencies>;
  readonly #ledger: RootLedger;
  readonly #token = {};
  readonly #slots = new Map<string, Slot>();
  readonly #routes = new Set<RouteState>();
  readonly #closing = new WeakMap<BoundSandbox, Promise<void>>();
  readonly #worldDisposals = new WeakMap<NativeProviderWorld, Promise<void>>();
  #closed = false;
  #disposing: Promise<void> | undefined;

  constructor(rootIdentity: object, config: NativeConfig, isLive: (agent: Agent) => boolean, dependencies: NativeOwnerDependencies = {}) {
    this.#config = validateNativeConfig(config); this.#isLive = isLive;
    const endpoint = new URL(`${this.#config.protocol}://${this.#config.domain}`);
    this.#destination = createHash('sha256').update(JSON.stringify([endpoint.origin, this.#config.remoteCwd])).digest('hex');
    this.#dependencies = { openBinding: dependencies.openBinding ?? openBinding, createProviders: dependencies.createProviders ?? createProviderWorld };
    const root = rootIdentity as { [ledgerKey]?: RootLedger };
    let ledger = root[ledgerKey];
    if (!ledger) { ledger = { version: 1, entries: new Map() }; Object.defineProperty(root, ledgerKey, { value: ledger }); }
    if (ledger.version !== 1 || ledger.active) throw error('owner_busy');
    this.#ledger = ledger; ledger.active = this.#token;
    for (const record of ledger.entries.values()) {
      if (record.state === 'opening') { record.state = 'outcome-unknown'; record.failure = 'outcome_unknown'; }
      else if (['open', 'unopened', 'unadmitted'].includes(record.state)) record.state = 'reconnect-required';
    }
  }

  attach(agent: Agent): NativeRoute {
    if (this.#closed) throw error('owner_closed');
    const sessionId = String(agent.id);
    if (!sessionId || sessionId.length > 512 || /[\x00-\x1f\x7f]/.test(sessionId)) throw error('inactive_route');
    let slot = this.#slots.get(sessionId); let fresh = false;
    if (!slot) {
      let record = this.#ledger.entries.get(sessionId);
      if (!record) { fresh = true; record = { state: 'unadmitted', revision: 0, destination: this.#destination }; this.#ledger.entries.set(sessionId, record); }
      slot = { sessionId, record, generation: 0 }; this.#slots.set(sessionId, slot);
    }
    if (slot.route && !slot.route.disposed) throw error('inactive_route');
    const route: RouteState = { agent, slot, generation: ++slot.generation, fresh, disposed: false,
      ...(slot.route?.draining || slot.previousDrain ? { predecessor: slot.route?.draining ?? slot.previousDrain! } : {}) };
    delete slot.previousDrain; slot.route = route; this.#routes.add(route);
    // This effect is awaited by the Agent scope; agent/disposed notifications are not awaited.
    agent.ctx.effect(() => () => this.#disposeRoute(route), 'OpenSandbox native provider drain');
    return Object.freeze({
      sessionId, remoteCwd: this.#config.remoteCwd,
      assertActive: () => this.#assertActive(route), acquire: (signal?: AbortSignal) => this.#acquire(route, signal),
      peekProviders: () => { this.#assertActive(route); return route.providers; },
      status: () => { this.#assertRoute(route); return this.#status(slot); },
      reconnect: (sandboxId: string, signal?: AbortSignal) => this.#reconnect(route, sandboxId, signal),
      delete: (sandboxId: string) => this.#delete(route, sandboxId), dispose: () => this.#disposeRoute(route),
    });
  }

  observeCreated(agent: Agent, source: 'startup' | 'resume'): void {
    if (this.#closed) return;
    const slot = this.#slots.get(String(agent.id)); const route = slot?.route;
    if (!slot || !route || route.agent !== agent || route.disposed || !this.#isLive(agent)) return;
    if (slot.record.state === 'unadmitted') slot.record.state = source === 'startup' && route.fresh ? 'unopened' : 'reconnect-required';
  }

  #assertRoute(route: RouteState): void {
    if (this.#closed || this.#ledger.active !== this.#token) throw error('owner_closed');
    if (route.disposed || route.slot.route !== route || route.slot.generation !== route.generation || !this.#isLive(route.agent)) throw error('inactive_route');
  }
  #assertActive(route: RouteState): void {
    this.#assertRoute(route);
    if (route.slot.operation) throw error('operation_pending');
  }
  #status(slot: Slot): NativeStatus {
    const { record } = slot; const destinationMatches = record.destination === this.#destination;
    return Object.freeze({ sessionId: slot.sessionId, remoteCwd: this.#config.remoteCwd, destinationMatches,
      state: destinationMatches ? record.state : 'configuration-mismatch',
      ...(destinationMatches && record.descriptor ? { sandboxId: record.descriptor.sandboxId } : {}),
      remoteExistence: destinationMatches && record.cleanupState === 'killed' ? 'deleted' : 'unverified',
      ...(destinationMatches && record.diagnostic ? record.diagnostic : {}),
      ...(slot.operation ? { operation: slot.operation } : {}), ...(record.cleanupState ? { cleanupState: record.cleanupState } : {}),
      ...(record.failure ? { failure: record.failure } : {}) });
  }
  #assertDestination(slot: Slot): void {
    if (slot.record.destination !== this.#destination) throw error('configuration_mismatch');
  }
  #unavailable(record: RecordEntry): NativeOwnerError {
    if (record.state === 'unadmitted') return error('not_admitted');
    if (record.state === 'reconnect-required') return error('reconnect_required');
    if (record.state === 'outcome-unknown') return error('outcome_unknown');
    if (record.state === 'drain-failed') return error('drain_failed');
    return error('binding_unavailable');
  }
  #openOptions(slot: Slot, sandboxId?: string): OpenOptions {
    const c = this.#config;
    const common = { sessionId: slot.sessionId, remoteCwd: c.remoteCwd,
      connectionConfig: { domain: c.domain, protocol: c.protocol, ...(c.apiKey ? { apiKey: c.apiKey } : {}), requestTimeoutSeconds: c.requestTimeoutSeconds, disableMetrics: true },
      signal: slot.abort!.signal, readyTimeoutSeconds: c.readyTimeoutSeconds, preflightTimeoutSeconds: c.preflightTimeoutSeconds };
    return sandboxId ? { ...common, kind: 'connect', sandboxId } : c.kind === 'create-template'
      ? { ...common, kind: 'create-template', templateId: c.templateId!, timeoutSeconds: c.ttlSeconds }
      : { ...common, kind: 'create-image', image: c.image!, timeoutSeconds: c.ttlSeconds };
  }
  #startOpening(slot: Slot, sandboxId?: string): Promise<BoundSandbox> {
    const record = slot.record; const revision = ++record.revision;
    slot.abort = new AbortController(); record.state = 'opening'; delete record.failure; delete record.cleanupState; delete record.diagnostic;
    const opening = (async () => {
      let binding: BoundSandbox;
      try { binding = await this.#dependencies.openBinding(this.#openOptions(slot, sandboxId)); }
      catch (failure) {
        if (record.revision === revision) {
          record.diagnostic = safeFailureFacts(failure);
          if (failure instanceof BindingOpenError && validId(failure.sandboxId)) {
            record.descriptor = Object.freeze({ version: 1, sessionId: slot.sessionId, sandboxId: failure.sandboxId, remoteCwd: this.#config.remoteCwd });
            const cleanup = ['killed', 'cleanup-unknown', 'not-owned'].includes(failure.cleanupState) ? failure.cleanupState : sandboxId ? 'not-owned' : 'cleanup-unknown';
            record.cleanupState = cleanup;
            record.state = cleanup === 'killed' ? 'killed' : cleanup === 'cleanup-unknown' ? 'cleanup-unknown' : 'reconnect-required';
            record.failure = 'binding_open_failed';
          } else { record.state = sandboxId ? 'reconnect-required' : 'outcome-unknown'; record.failure = sandboxId ? 'reconnect_failed' : 'outcome_unknown'; }
        }
        if (this.#closed) throw error('owner_closed');
        throw error(failure instanceof BindingOpenError ? 'binding_open_failed' : sandboxId ? 'reconnect_failed' : 'outcome_unknown');
      }
      if (binding.descriptor.sessionId !== slot.sessionId || binding.descriptor.remoteCwd !== this.#config.remoteCwd || !validId(binding.descriptor.sandboxId) || (sandboxId && binding.descriptor.sandboxId !== sandboxId)) {
        await this.#closeBinding(binding).catch(() => undefined);
        if (record.revision === revision) { record.state = sandboxId ? 'reconnect-required' : 'outcome-unknown'; record.failure = sandboxId ? 'reconnect_failed' : 'outcome_unknown'; }
        throw error(sandboxId ? 'reconnect_failed' : 'outcome_unknown');
      }
      if (record.revision === revision) record.descriptor = Object.freeze({ ...binding.descriptor });
      if (this.#closed || this.#ledger.active !== this.#token || record.revision !== revision) {
        if (record.revision === revision && record.state !== 'drain-failed') record.state = 'reconnect-required';
        await this.#closeBinding(binding); throw error('owner_closed');
      }
      slot.binding = binding;
      if (record.state !== 'drain-failed') record.state = binding.state === 'open' ? 'open' : 'failed';
      return binding;
    })();
    slot.opening = opening;
    // Observe even when every caller stops waiting; retain one outcome, never retry create.
    void opening.catch(() => undefined);
    return opening;
  }
  async #acquire(route: RouteState, signal?: AbortSignal): Promise<NativeProviders> {
    if (signal?.aborted) throw error('aborted'); this.#assertActive(route); this.#assertDestination(route.slot);
    if (route.predecessor) { await waitFor(route.predecessor, signal); this.#assertActive(route); delete route.predecessor; }
    const slot = route.slot;
    if (!slot.binding && !slot.opening && slot.record.state !== 'unopened') throw this.#unavailable(slot.record);
    if (slot.record.state !== 'open' && slot.record.state !== 'unopened' && slot.record.state !== 'opening') throw this.#unavailable(slot.record);
    route.preparing ??= this.#prepare(route);
    const providers = await waitFor(route.preparing, signal); this.#assertActive(route);
    if (providers.binding.state !== 'open' || slot.record.state !== 'open') throw error('binding_unavailable');
    return providers;
  }
  async #prepare(route: RouteState): Promise<NativeProviderWorld> {
    const slot = route.slot;
    const binding = slot.binding ?? await (slot.opening ?? this.#startOpening(slot)); this.#assertActive(route);
    if (binding.state !== 'open' || slot.record.state !== 'open') throw this.#unavailable(slot.record);
    let world: NativeProviderWorld;
    route.creatingProviders = true;
    try {
      try { world = await this.#dependencies.createProviders(route.agent, binding, this.#config, () => this.#assertActive(route)); }
      catch (failure) {
        if (failure instanceof NativeOwnerError) {
          if (failure.code === 'drain_failed') { slot.record.state = 'drain-failed'; slot.record.failure = 'drain_failed'; }
          else if (failure.code === 'provider_failed') { slot.record.state = 'failed'; slot.record.failure = 'provider_failed'; }
          throw failure;
        }
        slot.record.state = 'failed'; slot.record.failure = 'provider_failed'; throw error('provider_failed');
      }
      try { this.#assertActive(route); }
      catch (failure) { await this.#disposeWorld(world, slot); throw failure; }
      route.providers = world; return world;
    } finally { route.creatingProviders = false; }
  }
  #disposeWorld(world: NativeProviderWorld, slot: Slot): Promise<void> {
    let disposal = this.#worldDisposals.get(world);
    if (!disposal) {
      disposal = bounded(Promise.resolve().then(() => world.dispose()), this.#config.settlementTimeoutMs, 'drain_failed').catch(() => {
        slot.record.state = 'drain-failed'; slot.record.failure = 'drain_failed'; throw error('drain_failed');
      });
      this.#worldDisposals.set(world, disposal); void disposal.catch(() => undefined);
    }
    return disposal;
  }
  async #drainProviders(route: RouteState): Promise<void> {
    const world = route.providers; delete route.providers;
    const results = await Promise.allSettled([
      ...(world ? [this.#disposeWorld(world, route.slot)] : []),
      ...(route.preparing && route.creatingProviders ? [bounded(route.preparing.then(() => undefined, () => undefined), this.#config.settlementTimeoutMs, 'drain_failed')] : []),
    ]);
    delete route.preparing;
    if (results.some(result => result.status === 'rejected') || route.slot.record.state === 'drain-failed') {
      route.slot.record.state = 'drain-failed'; route.slot.record.failure = 'drain_failed'; throw error('drain_failed');
    }
  }
  #disposeRoute(route: RouteState): Promise<void> {
    if (route.draining) return route.draining;
    route.disposed = true;
    route.draining = this.#drainProviders(route).finally(() => {
      this.#routes.delete(route);
      if (route.slot.route === route) {
        // Retain only the safe drain outcome, not the disposed Agent/provider world.
        route.slot.previousDrain = route.draining!; delete route.slot.route;
      }
    });
    void route.draining.catch(() => undefined); return route.draining;
  }
  #closeBinding(binding: BoundSandbox): Promise<void> {
    let closing = this.#closing.get(binding);
    if (!closing) {
      closing = bounded(Promise.resolve().then(() => binding.close()), this.#config.settlementTimeoutMs, 'close_failed').catch(() => { throw error('close_failed'); });
      this.#closing.set(binding, closing); void closing.catch(() => undefined);
    }
    return closing;
  }

  async #delete(route: RouteState, sandboxId: string): Promise<NativeStatus> {
    this.#assertRoute(route); const slot = route.slot; this.#assertDestination(slot);
    if (!validId(sandboxId)) throw error('invalid_sandbox_id');
    if (slot.record.descriptor && slot.record.descriptor.sandboxId !== sandboxId) throw error('sandbox_id_mismatch');
    if (slot.operation) {
      if (slot.operation === 'delete' && slot.mutationId === sandboxId && slot.mutation) { await slot.mutation; this.#assertRoute(route); return this.#status(slot); }
      throw error('operation_pending');
    }
    if (!slot.binding && !slot.opening && !slot.record.descriptor) throw error('binding_required');
    if (slot.record.state === 'delete-pending' || (slot.record.state === 'cleanup-unknown' && !slot.binding)) throw error('deletion_unresolved');
    if (slot.record.state === 'killed') return this.#status(slot);
    slot.operation = 'delete'; slot.mutationId = sandboxId;
    const mutation = (async () => {
      if (slot.opening) { await slot.opening.catch(() => undefined); this.#assertRoute(route); }
      if (slot.record.descriptor?.sandboxId !== sandboxId) throw error('sandbox_id_mismatch');
      if (slot.record.state === 'killed') return this.#status(slot);
      if (!slot.binding) throw error('reconnect_required');
      // A confirmed explicit DELETE may stop commands whose provider drain could not confirm termination.
      // Drain failure blocks reuse, but must not prevent this independent, authorized delete attempt.
      await this.#drainProviders(route).catch(() => undefined); this.#assertRoute(route);
      // Persist destructive intent before dispatch. A bounded shutdown must not turn an
      // unresolved DELETE into a reconnectable ordinary closed binding on the next owner.
      const revision = ++slot.record.revision;
      slot.record.state = 'delete-pending'; slot.record.cleanupState = 'cleanup-unknown';
      const stillOwns = () => slot.record.revision === revision && this.#ledger.active === this.#token;
      try { await slot.binding.kill(); }
      catch {
        if (stillOwns()) { slot.record.state = 'cleanup-unknown'; slot.record.cleanupState = 'cleanup-unknown'; slot.record.failure = 'cleanup_unknown'; }
        this.#assertRoute(route); throw error('cleanup_unknown');
      }
      if (stillOwns()) { slot.record.state = 'killed'; slot.record.cleanupState = 'killed'; delete slot.record.failure; }
      this.#assertRoute(route); return this.#status(slot);
    })().finally(() => { delete slot.operation; delete slot.mutation; delete slot.mutationId; });
    slot.mutation = mutation; void mutation.catch(() => undefined);
    await mutation; this.#assertRoute(route); return this.#status(slot);
  }
  async #reconnect(route: RouteState, sandboxId: string, signal?: AbortSignal): Promise<NativeStatus> {
    if (signal?.aborted) throw error('aborted'); this.#assertRoute(route); const slot = route.slot; this.#assertDestination(slot);
    if (!validId(sandboxId)) throw error('invalid_sandbox_id');
    if (slot.record.state === 'drain-failed') throw error('drain_failed');
    if (slot.record.state === 'delete-pending' || slot.record.state === 'cleanup-unknown') throw error('deletion_unresolved');
    if (slot.record.descriptor && slot.record.descriptor.sandboxId !== sandboxId) throw error('sandbox_id_mismatch');
    if (slot.operation) throw error('operation_pending');
    slot.operation = 'reconnect'; slot.mutationId = sandboxId;
    const mutation = (async () => {
      if (slot.opening) { await slot.opening.catch(() => undefined); this.#assertRoute(route); }
      if (slot.record.descriptor && slot.record.descriptor.sandboxId !== sandboxId) throw error('sandbox_id_mismatch');
      await this.#drainProviders(route); this.#assertRoute(route);
      if (slot.binding) { await this.#closeBinding(slot.binding); this.#assertRoute(route); delete slot.binding; }
      delete slot.opening;
      await this.#startOpening(slot, sandboxId); this.#assertRoute(route);
      return this.#status(slot);
    })().finally(() => { delete slot.operation; delete slot.mutation; delete slot.mutationId; });
    slot.mutation = mutation; void mutation.catch(() => undefined);
    await waitFor(mutation, signal); this.#assertRoute(route); return this.#status(slot);
  }

  dispose(): Promise<void> {
    if (this.#disposing) return this.#disposing;
    // Synchronous admission closure precedes every await, including provider and SDK cleanup.
    this.#closed = true;
    for (const slot of this.#slots.values()) slot.abort?.abort();
    const drains = [...this.#routes].map(route => this.#disposeRoute(route));
    this.#disposing = (async () => {
      const waits = [...this.#slots.values()].flatMap(slot => [
        ...(slot.opening ? [bounded(slot.opening.then(() => undefined, () => undefined), this.#config.settlementTimeoutMs, 'disposal_incomplete')] : []),
        ...(slot.mutation ? [bounded(slot.mutation.then(() => undefined, () => undefined), this.#config.settlementTimeoutMs, 'disposal_incomplete')] : []),
      ]);
      const drained = await Promise.allSettled([...drains, ...waits]);
      const closed = await Promise.allSettled([...this.#slots.values()].map(async slot => {
        const binding = slot.binding; delete slot.binding;
        if (slot.record.state === 'open') slot.record.state = 'reconnect-required';
        // Drop settled/live owner references even if the independently attempted close fails.
        delete slot.opening; delete slot.abort; delete slot.mutation;
        if (binding) {
          try { await this.#closeBinding(binding); }
          catch { slot.record.failure = 'close_failed'; throw error('close_failed'); }
        }
      }));
      this.#routes.clear();
      if (this.#ledger.active === this.#token) delete this.#ledger.active;
      if ([...drained, ...closed].some(result => result.status === 'rejected') || [...this.#slots.values()].some(slot => slot.record.state === 'drain-failed')) throw error('disposal_incomplete');
    })();
    void this.#disposing.catch(() => undefined); return this.#disposing;
  }
}
