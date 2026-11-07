// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

import { afterEach, describe, expect, it, vi } from 'vitest';
import type { Agent } from '@deepseek-ai/dsh-agent';
import type { BoundSandbox, OpenOptions } from '../src/core/types.js';
import { BindingOpenError, UnknownOutcomeError } from '../src/core/errors.js';
import { NativeOwner } from '../src/owner.js';
import { createBindingOpener } from '../src/core/binding.js';
import { fakeSandbox, fakeSdk } from './core-fixtures/sdk-facade.js';
import type { NativeConfig } from '../src/config.js';
import type { NativeOwnerDependencies, NativeProviderWorld } from '../src/owner.js';

const config = { domain: 'private.example', protocol: 'https' as const, apiKey: 'do-not-leak', kind: 'create-template' as const, templateId: 'template', settlementTimeoutMs: 20 };
function deferred<T>() { let resolve!: (value: T) => void, reject!: (error: unknown) => void; const promise = new Promise<T>((yes, no) => { resolve = yes; reject = no; }); return { promise, resolve, reject }; }
function fakeBinding(sessionId: string, sandboxId = `sandbox-${sessionId}`): BoundSandbox {
  let state: BoundSandbox['state'] = 'open';
  const descriptor = Object.freeze({ version: 1 as const, sessionId, sandboxId, remoteCwd: '/tmp' });
  return { descriptor, sandbox: {} as BoundSandbox['sandbox'], get state() { return state; },
    close: vi.fn(async () => { state = 'closed'; }), kill: vi.fn(async () => { state = 'killed'; }), toJSON: () => descriptor };
}
function fixture(root = {}, overrides: NativeOwnerDependencies = {}, configuration: NativeConfig = config) {
  const live = new Map<string, Agent>(); const effects: Array<() => Promise<void>> = [];
  const openBinding = vi.fn(async (options: OpenOptions) => fakeBinding(options.sessionId, options.kind === 'connect' ? options.sandboxId : undefined));
  const worlds: NativeProviderWorld[] = [];
  const createProviders = vi.fn(async (_agent: Agent, binding: BoundSandbox): Promise<NativeProviderWorld> => {
    const world = { binding, shell: {} as NativeProviderWorld['shell'], fs: {} as NativeProviderWorld['fs'], dispose: vi.fn(async () => undefined) };
    worlds.push(world); return world;
  });
  const owner = new NativeOwner(root, configuration, agent => live.get(String(agent.id)) === agent, { openBinding, createProviders, ...overrides });
  const attach = (id: string, source?: 'startup' | 'resume') => {
    const agent = { id, ctx: { effect: (effect: () => () => Promise<void>) => { effects.push(effect()); } } } as unknown as Agent;
    const route = owner.attach(agent); live.set(id, agent); if (source) owner.observeCreated(agent, source);
    return { agent, route };
  };
  return { owner, attach, live, effects, openBinding, worlds, createProviders };
}
afterEach(() => vi.useRealTimers());

describe('native session owner', () => {
  it('is lazy, creates once for concurrent shell/file callers, and keeps per-session isolation', async () => {
    const f = fixture(); const a = f.attach('a', 'startup'); const b = f.attach('b', 'startup');
    expect(f.openBinding).not.toHaveBeenCalled(); expect(a.route.status().state).toBe('unopened');
    const [shell, file] = await Promise.all([a.route.acquire(), a.route.acquire()]);
    expect(shell).toBe(file); expect(a.route.peekProviders()).toBe(shell);
    expect((await a.route.acquire()).binding).toBe(shell.binding); await b.route.acquire();
    expect(f.openBinding).toHaveBeenCalledTimes(2); expect(f.createProviders).toHaveBeenCalledTimes(2);
    expect(f.openBinding.mock.calls[0]![0].connectionConfig).toMatchObject({ domain: 'private.example', apiKey: 'do-not-leak' });
    expect(JSON.stringify(a.route.status())).not.toMatch(/private.example|do-not-leak|template/);
    expect(a.route.status().remoteExistence).toBe('unverified'); await f.owner.dispose();
  });

  it('admits startup only after publication and refuses cold resume, including unused sessions', async () => {
    const f = fixture(); const pending = f.attach('pending');
    await expect(pending.route.acquire()).rejects.toMatchObject({ code: 'not_admitted' });
    f.owner.observeCreated(pending.agent, 'startup'); await pending.route.acquire();
    const restored = f.attach('restored', 'resume');
    await expect(restored.route.acquire()).rejects.toMatchObject({ code: 'reconnect_required' });
    expect(f.openBinding).toHaveBeenCalledTimes(1); await f.owner.dispose();
  });

  it('caller cancellation never aborts shared creation or other waiters', async () => {
    const opening = deferred<BoundSandbox>(); const openBinding = vi.fn((_options: OpenOptions) => opening.promise);
    const f = fixture({}, { openBinding }); const { route } = f.attach('shared', 'startup');
    const abort = new AbortController(); const one = route.acquire(abort.signal); const two = route.acquire();
    abort.abort(); await expect(one).rejects.toMatchObject({ code: 'aborted' });
    expect(openBinding.mock.calls[0]![0].signal?.aborted).toBe(false);
    opening.resolve(fakeBinding('shared')); expect((await two).binding.descriptor.sandboxId).toBe('sandbox-shared');
    expect(openBinding).toHaveBeenCalledTimes(1); await f.owner.dispose();
  });

  it('pre-aborted caller starts no creation', async () => {
    const f = fixture(); const { route } = f.attach('aborted', 'startup');
    await expect(route.acquire(AbortSignal.abort())).rejects.toMatchObject({ code: 'aborted' });
    expect(f.openBinding).not.toHaveBeenCalled(); await f.owner.dispose();
  });

  it('latches unknown creation across callers and ordinary owner remount', async () => {
    const root = {}; const f = fixture(root, { openBinding: vi.fn(async () => { throw new UnknownOutcomeError('create', 'request-safe', undefined, new Error('secret raw SDK')); }) });
    const { route } = f.attach('unknown', 'startup');
    await expect(route.acquire()).rejects.toMatchObject({ code: 'outcome_unknown' });
    await expect(route.acquire()).rejects.toMatchObject({ code: 'outcome_unknown' });
    expect(route.status()).toMatchObject({ state: 'outcome-unknown', correlationId: 'request-safe', classification: 'transport' }); expect(JSON.stringify(route.status())).not.toContain('secret');
    await f.owner.dispose(); const next = fixture(root); const resumed = next.attach('unknown', 'resume');
    await expect(resumed.route.acquire()).rejects.toMatchObject({ code: 'outcome_unknown' });
    expect(next.openBinding).not.toHaveBeenCalled(); await next.owner.dispose();
  });

  it('same-owner resume drains the old provider world and reuses only its binding', async () => {
    const f = fixture(); const a = f.attach('resume', 'startup'); const first = await a.route.acquire();
    await a.route.dispose(); const b = f.attach('resume', 'resume'); const second = await b.route.acquire();
    expect(second.binding).toBe(first.binding); expect(second.shell).not.toBe(first.shell);
    expect(f.openBinding).toHaveBeenCalledTimes(1); expect(f.worlds[0]!.dispose).toHaveBeenCalledOnce();
    await expect(a.route.acquire()).rejects.toMatchObject({ code: 'inactive_route' }); await f.owner.dispose();
  });

  it('rejects foreign Agent identity after opening and never composes providers', async () => {
    const opening = deferred<BoundSandbox>(); const f = fixture({}, { openBinding: () => opening.promise });
    const { agent, route } = f.attach('identity', 'startup'); const pending = route.acquire();
    f.live.set('identity', { ...agent } as Agent); opening.resolve(fakeBinding('identity'));
    await expect(pending).rejects.toMatchObject({ code: 'inactive_route' }); expect(f.createProviders).not.toHaveBeenCalled();
    await f.owner.dispose();
  });

  it('checks exact route again after provider preparation and drains late providers', async () => {
    const preparing = deferred<NativeProviderWorld>(); const f = fixture({}, { createProviders: () => preparing.promise });
    const { agent, route } = f.attach('late-provider', 'startup'); const pending = route.acquire(); await Promise.resolve(); await Promise.resolve();
    f.live.set('late-provider', { ...agent } as Agent);
    const world = { binding: fakeBinding('late-provider'), shell: {}, fs: {}, dispose: vi.fn(async () => undefined) } as unknown as NativeProviderWorld;
    preparing.resolve(world); await expect(pending).rejects.toMatchObject({ code: 'inactive_route' });
    expect(world.dispose).toHaveBeenCalledOnce(); await f.owner.dispose();
  });

  it('status and delete never open an idle binding', async () => {
    const f = fixture(); const { route } = f.attach('idle', 'startup'); route.status();
    await expect(route.delete('some-id')).rejects.toMatchObject({ code: 'binding_required' });
    expect(f.openBinding).not.toHaveBeenCalled(); await f.owner.dispose();
  });

  it('explicit reconnect connects only, and a failed connect never creates', async () => {
    const f = fixture(); const { route } = f.attach('cold', 'resume');
    await route.reconnect('existing-id'); expect(f.openBinding.mock.calls[0]![0]).toMatchObject({ kind: 'connect', sandboxId: 'existing-id' });
    expect(route.status().sandboxId).toBe('existing-id'); await route.acquire(); expect(f.openBinding).toHaveBeenCalledOnce(); await f.owner.dispose();
    const bad = fixture({}, { openBinding: vi.fn(async () => { throw new Error('raw key/domain'); }) }); const cold = bad.attach('bad', 'resume');
    await expect(cold.route.reconnect('missing')).rejects.toMatchObject({ code: 'reconnect_failed' });
    await expect(cold.route.acquire()).rejects.toMatchObject({ code: 'reconnect_required' }); await bad.owner.dispose();
  });

  it('serializes explicit delete with opening and never recreates a killed binding', async () => {
    const opening = deferred<BoundSandbox>(); const binding = fakeBinding('delete');
    const f = fixture({}, { openBinding: () => opening.promise }); const { route } = f.attach('delete', 'startup');
    const acquire = route.acquire(); const deleting = route.delete('sandbox-delete');
    opening.resolve(binding); await expect(acquire).rejects.toMatchObject({ code: 'operation_pending' });
    expect((await deleting).state).toBe('killed'); expect(binding.kill).toHaveBeenCalledOnce();
    await expect(route.acquire()).rejects.toMatchObject({ code: 'binding_unavailable' }); await f.owner.dispose();
  });

  it('does not repeat readiness-failure cleanup DELETE, and preserves sanitized evidence', async () => {
    const f = fixture({}, { openBinding: async () => { throw new BindingOpenError('readiness', 'allocated-id', 'killed'); } });
    const { route } = f.attach('cleanup', 'startup'); await expect(route.acquire()).rejects.toMatchObject({ code: 'binding_open_failed' });
    expect(route.status()).toMatchObject({ state: 'killed', sandboxId: 'allocated-id', cleanupState: 'killed' });
    expect((await route.delete('allocated-id')).state).toBe('killed'); await f.owner.dispose();
  });

  it('requires matching delete ID, retains unknown deletion, and retries only explicitly', async () => {
    const binding = fakeBinding('retry'); vi.mocked(binding.kill).mockRejectedValueOnce(new Error('raw secret'));
    const f = fixture({}, { openBinding: async () => binding }); const { route } = f.attach('retry', 'startup'); await route.acquire();
    await expect(route.delete('wrong')).rejects.toMatchObject({ code: 'sandbox_id_mismatch' }); expect(binding.kill).not.toHaveBeenCalled();
    await expect(route.delete('sandbox-retry')).rejects.toMatchObject({ code: 'cleanup_unknown' });
    expect(route.status().state).toBe('cleanup-unknown'); await expect(route.acquire()).rejects.toMatchObject({ code: 'binding_unavailable' });
    expect(binding.kill).toHaveBeenCalledOnce(); await route.delete('sandbox-retry'); expect(binding.kill).toHaveBeenCalledTimes(2); await f.owner.dispose();
  });

  it('closes admission synchronously, aborts owned opening, and closes late bindings without tools', async () => {
    vi.useFakeTimers(); const opening = deferred<BoundSandbox>(); const openBinding = vi.fn((_options: OpenOptions) => opening.promise);
    const f = fixture({}, { openBinding }); const { route } = f.attach('late', 'startup'); const pending = route.acquire();
    const settled = expect(pending).rejects.toMatchObject({ code: 'owner_closed' });
    const disposing = f.owner.dispose(); const closed = expect(disposing).rejects.toMatchObject({ code: 'disposal_incomplete' });
    expect(() => route.assertActive()).toThrow(); expect(openBinding.mock.calls[0]![0].signal?.aborted).toBe(true);
    await vi.advanceTimersByTimeAsync(25); await closed;
    const binding = fakeBinding('late'); opening.resolve(binding); await settled; await Promise.resolve();
    expect(binding.close).toHaveBeenCalledOnce(); expect(binding.kill).not.toHaveBeenCalled(); expect(f.createProviders).not.toHaveBeenCalled();
  });

  it('attempts independent local close even when provider draining fails and blocks reuse', async () => {
    const f = fixture(); const { route } = f.attach('drain', 'startup'); const { binding } = await route.acquire();
    vi.mocked(f.worlds[0]!.dispose).mockRejectedValue(new Error('secret drain detail'));
    await expect(route.dispose()).rejects.toMatchObject({ code: 'drain_failed' });
    const next = f.attach('drain', 'resume'); await expect(next.route.acquire()).rejects.toMatchObject({ code: 'drain_failed' });
    await expect(f.owner.dispose()).rejects.toMatchObject({ code: 'disposal_incomplete' }); expect(binding.close).toHaveBeenCalledOnce();
  });

  it('ordinary remount keeps the descriptor but never silently reconnects or recreates', async () => {
    const root = {}; const f = fixture(root); const a = f.attach('reload', 'startup'); await a.route.acquire(); await f.owner.dispose();
    const next = fixture(root); const b = next.attach('reload', 'resume'); expect(b.route.status().sandboxId).toBe('sandbox-reload');
    await expect(b.route.acquire()).rejects.toMatchObject({ code: 'reconnect_required' }); expect(next.openBinding).not.toHaveBeenCalled();
    await b.route.reconnect('sandbox-reload'); expect(next.openBinding.mock.calls[0]![0].kind).toBe('connect'); await next.owner.dispose();
  });

  it('Agent disposal during opening permits a new Agent to wait for the same binding without a false drain failure', async () => {
    const opening = deferred<BoundSandbox>(); const openBinding = vi.fn((_options: OpenOptions) => opening.promise);
    const f = fixture({}, { openBinding }); const old = f.attach('pending-resume', 'startup');
    const waiting = old.route.acquire(); const refused = expect(waiting).rejects.toMatchObject({ code: 'inactive_route' });
    await old.route.dispose(); const current = f.attach('pending-resume', 'resume'); const resumed = current.route.acquire();
    opening.resolve(fakeBinding('pending-resume')); await refused; await resumed;
    expect(openBinding).toHaveBeenCalledOnce(); expect(f.createProviders).toHaveBeenCalledOnce(); await f.owner.dispose();
  });

  it('does not replace a newly discovered ID when reconnect races creation', async () => {
    const opening = deferred<BoundSandbox>(); const f = fixture({}, { openBinding: () => opening.promise });
    const { route } = f.attach('race', 'startup'); const waiting = route.acquire();
    const refused = expect(waiting).rejects.toMatchObject({ code: 'operation_pending' });
    const reconnecting = route.reconnect('different-existing-id'); opening.resolve(fakeBinding('race'));
    await refused; await expect(reconnecting).rejects.toMatchObject({ code: 'sandbox_id_mismatch' });
    expect(route.status().sandboxId).toBe('sandbox-race'); await f.owner.dispose();
  });

  it('deduplicates simultaneous matching delete actions and allows explicit delete after failed drain', async () => {
    const f = fixture(); const { route } = f.attach('drain-delete', 'startup'); const { binding } = await route.acquire();
    vi.mocked(f.worlds[0]!.dispose).mockRejectedValue(new Error('unsafe details'));
    const [first, second] = await Promise.all([route.delete('sandbox-drain-delete'), route.delete('sandbox-drain-delete')]);
    expect(first.state).toBe('killed'); expect(second.state).toBe('killed'); expect(binding.kill).toHaveBeenCalledOnce();
    await f.owner.dispose();
  });

  it('rejects overlapping owners and never stores credentials or SDK objects in the root ledger', async () => {
    const root = {}; const f = fixture(root); const { route } = f.attach('root', 'startup'); await route.acquire();
    expect(() => fixture(root)).toThrow('Another OpenSandbox native owner is still active.');
    const ledger = (root as Record<symbol, unknown>)[Symbol.for('@opensandbox/deepseek-harness-native/ledger/v1')];
    const serialized = JSON.stringify(ledger, (_key, value) => value instanceof Map ? [...value.entries()] : value);
    expect(serialized).not.toMatch(/do-not-leak|private.example|sandbox":|template/); await f.owner.dispose();
  });


  it.each([{ domain: 'another.example' }, { protocol: 'http' as const }, { remoteCwd: '/another' }])('refuses all lifecycle reuse at a different destination or literal cwd', async change => {
    const root = {}; const first = fixture(root); await first.attach('destination', 'startup').route.acquire(); await first.owner.dispose();
    const changed = fixture(root, {}, { ...config, ...change }); const { route } = changed.attach('destination', 'resume');
    expect(route.status()).toMatchObject({ state: 'configuration-mismatch', destinationMatches: false });
    expect(route.status().sandboxId).toBeUndefined();
    await expect(route.acquire()).rejects.toMatchObject({ code: 'configuration_mismatch' });
    await expect(route.reconnect('sandbox-destination')).rejects.toMatchObject({ code: 'configuration_mismatch' });
    await expect(route.delete('sandbox-destination')).rejects.toMatchObject({ code: 'configuration_mismatch' });
    expect(changed.openBinding).not.toHaveBeenCalled(); await changed.owner.dispose();
    const restored = fixture(root); const saved = restored.attach('destination', 'resume');
    expect(saved.route.status()).toMatchObject({ destinationMatches: true, sandboxId: 'sandbox-destination' });
    await saved.route.reconnect('sandbox-destination'); await restored.owner.dispose();
  });

  it('retains only typed allowlisted diagnostics and the integration correlation token', async () => {
    const failure = new UnknownOutcomeError('create', 'generated-token');
    Object.assign(failure, { classification: 'api', statusCode: 503, sdkCode: 'FSB::API_ERROR', requestId: 'raw-private-request', cause: new Error('api-key-secret') });
    const f = fixture({}, { openBinding: async () => { throw failure; } }); const { route } = f.attach('diagnostic', 'startup');
    await expect(route.acquire()).rejects.toMatchObject({ code: 'outcome_unknown' });
    expect(route.status()).toMatchObject({ correlationId: 'generated-token', classification: 'api', statusCode: 503, sdkCode: 'FSB::API_ERROR' });
    expect(JSON.stringify(route.status())).not.toMatch(/raw-private-request|api-key-secret|cause|requestId/); await f.owner.dispose();
  });

  it('filters forged values even on the core error type', async () => {
    const failure = new UnknownOutcomeError('create', 'secret / unsafe');
    Object.assign(failure, { classification: 'raw secret', statusCode: 999, sdkCode: 'SECRET' });
    const f = fixture({}, { openBinding: async () => { throw failure; } }); const { route } = f.attach('forged', 'startup');
    await expect(route.acquire()).rejects.toMatchObject({ code: 'outcome_unknown' });
    expect(route.status().correlationId).toBeUndefined(); expect(route.status().classification).toBeUndefined();
    expect(route.status().statusCode).toBeUndefined(); expect(route.status().sdkCode).toBeUndefined(); await f.owner.dispose();
  });


  it('drains every resumed Agent world once while retaining one session binding', async () => {
    const f = fixture();
    for (let index = 0; index < 50; index++) {
      const { route } = f.attach('many-resumes', index === 0 ? 'startup' : 'resume');
      await route.acquire(); await route.dispose();
    }
    expect(f.openBinding).toHaveBeenCalledOnce(); expect(f.worlds).toHaveLength(50);
    await f.owner.dispose();
    for (const world of f.worlds) expect(world.dispose).toHaveBeenCalledOnce();
  });


  it('keeps a pending DELETE tombstone across bounded shutdown and refuses revival by a new owner', async () => {
    const deletion = deferred<void>(); const started = deferred<void>(); const binding = fakeBinding('pending-delete');
    vi.mocked(binding.kill).mockImplementation(async () => { started.resolve(); await deletion.promise; });
    const root = {}; const first = fixture(root, { openBinding: async () => binding }); const original = first.attach('pending-delete', 'startup');
    await original.route.acquire(); const deleting = original.route.delete('sandbox-pending-delete');
    const deletedResult = expect(deleting).rejects.toMatchObject({ code: 'owner_closed' }); await started.promise;
    await expect(first.owner.dispose()).rejects.toMatchObject({ code: 'disposal_incomplete' });
    const second = fixture(root); const replacement = second.attach('pending-delete', 'resume');
    expect(replacement.route.status().state).toBe('delete-pending');
    await expect(replacement.route.reconnect('sandbox-pending-delete')).rejects.toMatchObject({ code: 'deletion_unresolved' });
    await expect(replacement.route.acquire()).rejects.toMatchObject({ code: 'binding_unavailable' });
    deletion.resolve(); await deletedResult;
    expect(replacement.route.status().state).toBe('delete-pending'); expect(second.openBinding).not.toHaveBeenCalled(); await second.owner.dispose();
  });


  it('preserves the core setup-cleanup exception when owned shutdown cancels readiness', async () => {
    const sdk = fakeSandbox('readiness-id'); const ready = deferred<void>();
    sdk.boundary.waitUntilReady.mockImplementation(async options => {
      ready.resolve(); await new Promise<void>((_resolve, reject) => options?.signal?.addEventListener('abort', () => reject(new Error('raw readiness cancellation')), { once: true }));
    });
    const root = {}; const f = fixture(root, { openBinding: createBindingOpener(fakeSdk(sdk.sandbox)) });
    const { route } = f.attach('readiness', 'startup'); const acquiring = route.acquire();
    const result = expect(acquiring).rejects.toMatchObject({ code: 'owner_closed' }); await ready.promise;
    await f.owner.dispose(); await result;
    expect(sdk.boundary.kill).toHaveBeenCalledOnce(); expect(sdk.boundary.close).toHaveBeenCalledOnce();
    const next = fixture(root); const saved = next.attach('readiness', 'resume');
    expect(saved.route.status()).toMatchObject({ state: 'killed', sandboxId: 'readiness-id', cleanupState: 'killed', stage: 'readiness' });
    await saved.route.delete('readiness-id'); expect(next.openBinding).not.toHaveBeenCalled(); expect(sdk.boundary.kill).toHaveBeenCalledOnce(); await next.owner.dispose();
  });

  it('bounds provider preparation disposal, closes SDK independently, and drains a late world', async () => {
    const preparing = deferred<NativeProviderWorld>(); const started = deferred<void>(); const binding = fakeBinding('late-world');
    const f = fixture({}, { openBinding: async () => binding, createProviders: async () => { started.resolve(); return preparing.promise; } });
    const { route } = f.attach('late-world', 'startup'); const acquiring = route.acquire();
    const refused = expect(acquiring).rejects.toMatchObject({ code: 'owner_closed' }); await started.promise;
    await expect(f.owner.dispose()).rejects.toMatchObject({ code: 'disposal_incomplete' }); expect(binding.close).toHaveBeenCalledOnce();
    const world = { binding, shell: {}, fs: {}, dispose: vi.fn(async () => undefined) } as unknown as NativeProviderWorld;
    preparing.resolve(world); await refused; expect(world.dispose).toHaveBeenCalledOnce(); expect(binding.kill).not.toHaveBeenCalled();
  });

  it('bounds local SDK close independently and retains only a safe close failure', async () => {
    const binding = fakeBinding('close'); const closing = deferred<void>(); vi.mocked(binding.close).mockImplementation(() => closing.promise);
    const root = {}; const first = fixture(root, { openBinding: async () => binding }); await first.attach('close', 'startup').route.acquire();
    await expect(first.owner.dispose()).rejects.toMatchObject({ code: 'disposal_incomplete' });
    const second = fixture(root); const { route } = second.attach('close', 'resume');
    expect(route.status()).toMatchObject({ state: 'reconnect-required', failure: 'close_failed' });
    await expect(route.acquire()).rejects.toMatchObject({ code: 'reconnect_required' }); closing.resolve(); await second.owner.dispose();
  });

});
