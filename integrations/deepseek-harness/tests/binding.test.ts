// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

import { chmodSync, mkdirSync, mkdtempSync, readFileSync, rmSync, symlinkSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { spawnSync } from 'node:child_process';
import { ConnectionConfig } from '@alibaba-group/opensandbox';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { createBindingOpener, openBinding } from '../src/binding.js';
import { BindingError, BindingOpenError, UnknownOutcomeError } from '../src/errors.js';
import { SdkTransport } from '../src/sdk-transport.js';
import type { OpenOptions } from '../src/types.js';
import { deferred, execution, fakeSandbox, fakeSdk } from './fixtures/sdk-facade.js';
import { offlineSdkHttp } from './fixtures/offline-sdk.js';

const options = (overrides: Partial<OpenOptions> = {}): OpenOptions => ({
  kind: 'create-image', image: 'python:3.11', sessionId: 'session-a', remoteCwd: '/workspace',
  connectionConfig: { domain: 'sandbox.example', apiKey: 'do-not-serialize' },
  ...overrides,
} as OpenOptions);

afterEach(() => { vi.useRealTimers(); vi.unstubAllGlobals(); });

describe('bound lifecycle', () => {
  it('publishes an immutable versioned binding with the public SDK handle', async () => {
    const fake = fakeSandbox();
    const binding = await createBindingOpener(fakeSdk(fake.sandbox))(options());
    expect(binding.descriptor).toEqual({ version: 1, sessionId: 'session-a', sandboxId: 'sandbox-a', remoteCwd: '/workspace' });
    expect(binding.sandbox).toBe(fake.sandbox);
    expect(binding.state).toBe('open');
    expect(Object.isFrozen(binding.descriptor)).toBe(true);
    expect(() => Object.assign(binding.descriptor, { sandboxId: 'sandbox-b' })).toThrow();
    expect(() => Object.assign(binding, { descriptor: {}, sandbox: fakeSandbox('b').sandbox })).toThrow();
    expect(typeof openBinding).toBe('function');
  });

  it('prevents runtime property redefinition from replacing the binding or sandbox', async () => {
    const fake = fakeSandbox();
    const binding = await createBindingOpener(fakeSdk(fake.sandbox))(options());
    expect(() => Object.defineProperty(binding, 'descriptor', { value: { sandboxId: 'foreign' } })).toThrow();
    expect(() => Object.defineProperty(binding, 'sandbox', { value: fakeSandbox('foreign').sandbox })).toThrow();
    await binding.close();
    expect(binding.state).toBe('closed');
  });

  it('keeps identical paths in different sessions on their own sandbox', async () => {
    const a = fakeSandbox('a');
    const b = fakeSandbox('b');
    const ba = await createBindingOpener(fakeSdk(a.sandbox))(options());
    const bb = await createBindingOpener(fakeSdk(b.sandbox))(options({ sessionId: 'session-b' }));
    await new SdkTransport(ba.sandbox).writeBytes('/workspace/same', new Uint8Array([1]));
    await new SdkTransport(bb.sandbox).writeBytes('/workspace/same', new Uint8Array([2]));
    expect(a.files.writeFiles.mock.calls).toEqual([[[{ path: '/workspace/same', data: new Uint8Array([1]) }]]]);
    expect(b.files.writeFiles.mock.calls).toEqual([[[{ path: '/workspace/same', data: new Uint8Array([2]) }]]]);
    expect(ba.descriptor.sandboxId).toBe('a');
    expect(bb.descriptor.sandboxId).toBe('b');
  });

  it('waits for SDK readiness and remote prerequisites before publishing', async () => {
    const fake = fakeSandbox();
    const ready = deferred<void>();
    const preflight = deferred<ReturnType<typeof execution>>();
    fake.boundary.waitUntilReady.mockReturnValue(ready.promise);
    fake.commands.run.mockReturnValue(preflight.promise);
    let published = false;
    const sdk = fakeSdk(fake.sandbox);
    const pending = createBindingOpener(sdk)(options()).then(binding => { published = true; return binding; });
    await vi.waitFor(() => expect(fake.boundary.waitUntilReady).toHaveBeenCalledOnce());
    expect(fake.commands.run).not.toHaveBeenCalled();
    expect(published).toBe(false);
    expect(sdk.create.mock.calls[0]?.[0].skipHealthCheck).toBe(true);
    ready.resolve();
    await vi.waitFor(() => expect(fake.commands.run).toHaveBeenCalledOnce());
    expect(published).toBe(false);
    const [command, opts] = fake.commands.run.mock.calls[0]!;
    expect(command).toEqual(expect.arrayContaining(['bash', '-c']));
    expect(JSON.stringify(command)).toContain('python3');
    expect(JSON.stringify(command)).toContain('Linux');
    expect(opts?.workingDirectory).toBe('/');
    expect(command.at(-1)).toBe('/workspace');
    expect(opts?.background).toBe(false);
    preflight.resolve(execution());
    await pending;
    expect(published).toBe(true);
  });

  it('does not serialize or mutate caller-owned ConnectionConfig', async () => {
    const config = new ConnectionConfig({ domain: 'private.example', apiKey: 'do-not-serialize', headers: { Authorization: 'Bearer secret' } });
    const before = { apiKey: config.apiKey, headers: { ...config.headers } };
    const fake = fakeSandbox();
    const sdk = fakeSdk(fake.sandbox);
    const binding = await createBindingOpener(sdk)(options({ connectionConfig: config }));
    expect(sdk.create.mock.calls[0]?.[0].connectionConfig).toBe(config);
    expect(config.apiKey).toBe(before.apiKey);
    expect(config.headers).toEqual(before.headers);
    expect(JSON.parse(JSON.stringify(binding))).toEqual(binding.descriptor);
    expect(JSON.stringify(binding)).not.toMatch(/do-not-serialize|secret|private.example/);
  });

  it.each(['plain-options', 'uninitialized-instance', 'initialized-instance'] as const)('preserves actual SDK transport ownership for %s without mutating caller fields', async mode => {
    const http = offlineSdkHttp();
    const plain = { domain: 'offline.invalid', apiKey: 'test-only-key', headers: { 'X-Caller': 'unchanged' }, disableMetrics: true };
    const config = mode === 'plain-options' ? plain : mode === 'uninitialized-instance'
      ? new ConnectionConfig(plain) : new ConnectionConfig(plain).withTransportIfMissing();
    const fields = () => Object.fromEntries(Object.entries(config).filter(([key]) => !key.startsWith('_')).map(([key, value]) =>
      [key, key === 'headers' ? { ...value as Record<string, string> } : value]));
    const before = fields(), headers = config.headers;
    const make = (id: string) => openBinding({ kind: 'connect', sandboxId: id, sessionId: id, remoteCwd: '/workspace', connectionConfig: config });
    const a = await make('shared-a'), b = await make('shared-b');
    try {
      const shared = mode === 'initialized-instance';
      expect(a.sandbox.connectionConfig === b.sandbox.connectionConfig).toBe(shared);
      expect(a.sandbox.connectionConfig === config).toBe(shared);
      expect(a.sandbox.connectionConfig.fetch === b.sandbox.connectionConfig.fetch).toBe(shared);
      expect(fields()).toEqual(before); expect(config.headers).toBe(headers); expect(Object.isFrozen(config)).toBe(false);
      await a.close(); expect(b.state).toBe('open');
      const operation = new SdkTransport(b.sandbox).run(['true']);
      if (shared) await expect(operation).rejects.toMatchObject({ code: 'sdk_transport_failed' });
      else await expect(operation).resolves.toMatchObject({ exitCode: 0, complete: { executionTimeMs: 1 } });
      expect(http.closedAttempts).toBe(shared ? 1 : 0);
    } finally { await a.close(); await b.close(); }
    expect(fields()).toEqual(before); expect(config.headers).toBe(headers);
  });
  it('documents initialized transport sharing at the public option, close and lifecycle boundaries', () => {
    const source = readFileSync(new URL('../src/types.ts', import.meta.url), 'utf8');
    const close = source.slice(source.indexOf('export interface BoundSandbox'), source.indexOf('toJSON(): BindingDescriptor'));
    const option = source.slice(source.indexOf('interface BindingOptions'), source.indexOf('signal?: AbortSignal'));
    const guide = readFileSync(new URL('../../../docs/examples/deepseek-harness.md', import.meta.url), 'utf8');
    for (const text of [close, option, guide]) {
      expect(text).toContain('transport-initialized'); expect(text).toContain('dispatcher');
    }
    for (const phrase of ['uninitialized', 'plain options', 'failed opening', 'fresh configuration']) expect(guide).toContain(phrase);
  });
  it('passes an explicit template request with the required TTL', async () => {
    const fake = fakeSandbox();
    const sdk = fakeSdk(fake.sandbox);
    await createBindingOpener(sdk)({ kind: 'create-template', templateId: 'template-a', timeoutSeconds: 300,
      sessionId: 's', remoteCwd: '/workspace', connectionConfig: {} });
    expect(sdk.createFromTemplate).toHaveBeenCalledOnce();
    expect(sdk.createFromTemplate.mock.calls[0]?.[0]).toMatchObject({ templateId: 'template-a', timeoutSeconds: 300, skipHealthCheck: true });
    expect(sdk.create).not.toHaveBeenCalled();
    expect(sdk.connect).not.toHaveBeenCalled();
  });

  it.each(['readiness', 'preflight'] as const)('kills an owned create when %s fails', async stage => {
    const fake = fakeSandbox();
    if (stage === 'readiness') fake.boundary.waitUntilReady.mockRejectedValue(new Error('Bearer secret'));
    else fake.commands.run.mockResolvedValue(execution(127));
    const failure = await createBindingOpener(fakeSdk(fake.sandbox))(options()).catch(error => error);
    expect(failure).toBeInstanceOf(BindingOpenError);
    expect(failure).toMatchObject({ stage, sandboxId: 'sandbox-a', cleanupState: 'killed' });
    expect(fake.boundary.kill).toHaveBeenCalledOnce();
    expect(fake.boundary.close).toHaveBeenCalledOnce();
    expect(JSON.stringify(failure)).not.toContain('secret');
    expect(String(failure)).not.toContain('secret');
  });

  it('reports failed owned cleanup without claiming deletion', async () => {
    const fake = fakeSandbox();
    fake.commands.run.mockResolvedValue(execution(1));
    fake.boundary.kill.mockRejectedValue(new Error('api-key secret'));
    const failure = await createBindingOpener(fakeSdk(fake.sandbox))(options()).catch(error => error);
    expect(failure).toMatchObject({ code: 'binding_open_failed', cleanupState: 'cleanup-unknown', sandboxId: 'sandbox-a' });
    expect(fake.boundary.close).toHaveBeenCalledOnce();
    expect(JSON.stringify(failure)).not.toContain('secret');
  });

  it.each(['connect', 'readiness', 'preflight'] as const)('never kills an attached sandbox on %s failure', async stage => {
    const fake = fakeSandbox();
    const sdk = fakeSdk(fake.sandbox);
    if (stage === 'connect') sdk.connect.mockRejectedValue(new Error('connect secret'));
    else if (stage === 'readiness') fake.boundary.waitUntilReady.mockRejectedValue(new Error('ready secret'));
    else fake.commands.run.mockResolvedValue(execution(127));
    const failure = await createBindingOpener(sdk)({ kind: 'connect', sandboxId: 'existing-a', sessionId: 's',
      remoteCwd: '/workspace', connectionConfig: {} }).catch(error => error);
    expect(failure).toBeInstanceOf(BindingOpenError);
    expect(failure.cleanupState).toBe('not-owned');
    expect(fake.boundary.kill).not.toHaveBeenCalled();
    expect(fake.boundary.close).toHaveBeenCalledTimes(stage === 'connect' ? 0 : 1);
  });

  it('close terminally releases local resources once and refuses further deletion', async () => {
    const fake = fakeSandbox();
    const binding = await createBindingOpener(fakeSdk(fake.sandbox))(options());
    await Promise.all([binding.close(), binding.close()]);
    expect(binding.state).toBe('closed');
    expect(fake.boundary.close).toHaveBeenCalledOnce();
    expect(fake.boundary.kill).not.toHaveBeenCalled();
    await expect(binding.kill()).rejects.toMatchObject({ code: 'binding_closed', sandboxId: 'sandbox-a' });
    expect(binding.state).toBe('closed');
    expect(fake.boundary.kill).not.toHaveBeenCalled();
  });

  it('coalesces concurrent and repeated successful kill calls', async () => {
    const fake = fakeSandbox();
    const binding = await createBindingOpener(fakeSdk(fake.sandbox))(options());
    await Promise.all([binding.kill(), binding.kill()]);
    await binding.kill();
    expect(fake.boundary.kill).toHaveBeenCalledOnce();
    expect(binding.state).toBe('killed');
    await binding.close();
    expect(binding.state).toBe('killed');
  });

  it('retains unknown cleanup state and allows an explicit kill retry', async () => {
    const fake = fakeSandbox();
    fake.boundary.kill.mockRejectedValueOnce(new Error('lost DELETE response secret'));
    const binding = await createBindingOpener(fakeSdk(fake.sandbox))(options());
    await expect(binding.kill()).rejects.toMatchObject({ code: 'cleanup_unknown', sandboxId: 'sandbox-a' });
    expect(binding.state).toBe('cleanup-unknown');
    await binding.kill();
    expect(fake.boundary.kill).toHaveBeenCalledTimes(2);
    expect(binding.state).toBe('killed');
  });

  it('pins the real SDK identity and service references without freezing ConnectionConfig', async () => {
    const http = offlineSdkHttp();
    const sandbox = await http.connect('original');
    const binding = await createBindingOpener(fakeSdk(sandbox))({ kind: 'connect', sandboxId: 'original',
      sessionId: 'session-original', remoteCwd: '/workspace', connectionConfig: {} });
    try {
      const references = ['id', 'commands', 'files', 'sandboxes', 'connectionConfig'] as const;
      for (const key of references) {
        expect(() => Object.assign(sandbox, { [key]: key === 'id' ? 'foreign' : {} })).toThrow();
        expect(() => Object.defineProperty(sandbox, key, { value: key === 'id' ? 'foreign' : {} })).toThrow();
      }
      expect(Object.isFrozen(sandbox.connectionConfig)).toBe(false);
      const transport = new SdkTransport(binding.sandbox);
      await transport.run(['bash', '-c', 'true']);
      await transport.writeBytes('/workspace/same', new Uint8Array([1]));
      await binding.kill();
      expect(http.requests).toEqual(expect.arrayContaining([
        { url: 'http://offline.invalid/original/command', method: 'POST' },
        { url: 'http://offline.invalid/original/files/upload', method: 'POST' },
        { url: 'http://offline.invalid/v1/sandboxes/original', method: 'DELETE' },
      ]));
      expect(http.requests.some(request => request.url.includes('foreign'))).toBe(false);
      expect(binding.descriptor.sandboxId).toBe('original');
    } finally { await binding.close(); }
  });

  it('rejects post-close deletion locally before the real SDK sees a closed dispatcher', async () => {
    const http = offlineSdkHttp();
    const sandbox = await http.connect('closed');
    const binding = await createBindingOpener(fakeSdk(sandbox))({ kind: 'connect', sandboxId: 'closed',
      sessionId: 'closed-session', remoteCwd: '/workspace', connectionConfig: {} });
    await binding.close();
    await expect(binding.kill()).rejects.toMatchObject({ code: 'binding_closed', sandboxId: 'closed' });
    expect(http.closedAttempts).toBe(0);
    expect(http.requests.filter(request => request.method === 'DELETE')).toEqual([]);
    expect(binding.state).toBe('closed');
    // Recovery is an explicit new connection to the saved ID with a fresh SDK transport.
    const fresh = await http.connect(binding.descriptor.sandboxId);
    const recovered = await createBindingOpener(fakeSdk(fresh))({ kind: 'connect', sandboxId: binding.descriptor.sandboxId,
      sessionId: binding.descriptor.sessionId, remoteCwd: binding.descriptor.remoteCwd, connectionConfig: {} });
    try { await recovered.kill(); expect(recovered.state).toBe('killed'); }
    finally { await recovered.close(); }
    expect(http.requests.filter(request => request.method === 'DELETE')).toEqual([
      { url: 'http://offline.invalid/v1/sandboxes/closed', method: 'DELETE' },
    ]);
  });

  it('keeps prior cleanup uncertainty but rejects retry through a locally closed handle', async () => {
    const fake = fakeSandbox();
    fake.boundary.kill.mockRejectedValueOnce(new Error('lost DELETE response secret'));
    const binding = await createBindingOpener(fakeSdk(fake.sandbox))(options());
    await expect(binding.kill()).rejects.toMatchObject({ code: 'cleanup_unknown' });
    await binding.close();
    await expect(binding.kill()).rejects.toMatchObject({ code: 'binding_closed' });
    expect(binding.state).toBe('cleanup-unknown');
    expect(fake.boundary.kill).toHaveBeenCalledOnce();
  });

  it('reports lost create response as unknown with a non-sensitive correlation marker and no retry', async () => {
    const fake = fakeSandbox();
    const sdk = fakeSdk(fake.sandbox);
    sdk.create.mockRejectedValue(new Error('lost POST with do-not-serialize'));
    const metadata = Object.freeze({ project: 'integration-test' });
    const failure = await createBindingOpener(sdk)(options({ metadata })).catch(error => error);
    expect(failure).toBeInstanceOf(UnknownOutcomeError);
    expect(failure).toMatchObject({ operation: 'create', code: 'outcome_unknown' });
    expect(failure.correlationId).toMatch(/^[0-9a-f-]{36}$/);
    expect(sdk.create).toHaveBeenCalledOnce();
    expect(fake.boundary.kill).not.toHaveBeenCalled();
    expect(sdk.create.mock.calls[0]?.[0].metadata).toMatchObject({ project: 'integration-test', 'dsh-binding-request': failure.correlationId });
    expect(metadata).toEqual({ project: 'integration-test' });
    expect(String(failure)).not.toContain('do-not-serialize');
  });

  it('rejects pre-abort without launch and validates absolute remote cwd', async () => {
    const fake = fakeSandbox();
    const sdk = fakeSdk(fake.sandbox);
    const abort = new AbortController();
    abort.abort(new Error('secret'));
    await expect(createBindingOpener(sdk)(options({ signal: abort.signal }))).rejects.toMatchObject({ code: 'aborted' });
    await expect(createBindingOpener(sdk)(options({ remoteCwd: 'relative' }))).rejects.toBeInstanceOf(BindingError);
    expect(sdk.create).not.toHaveBeenCalled();
  });

  it('bounds prerequisite checks even if the SDK ignores abort after headers', async () => {
    vi.useFakeTimers();
    const fake = fakeSandbox();
    fake.commands.run.mockReturnValue(new Promise(() => {}));
    const pending = createBindingOpener(fakeSdk(fake.sandbox))(options({ preflightTimeoutSeconds: 1 }));
    const rejected = expect(pending).rejects.toMatchObject({ stage: 'preflight', cleanupState: 'killed' });
    await vi.advanceTimersByTimeAsync(1001);
    await rejected;
    expect(fake.boundary.kill).toHaveBeenCalledOnce();
  });

  it('keeps prerequisite output out of the SDK accumulation buffer', async () => {
    const fake = fakeSandbox();
    await createBindingOpener(fakeSdk(fake.sandbox))(options());
    expect(fake.commands.run.mock.calls[0]?.[2]?.skipAccumulation).toBe(true);
  });

  it.skipIf(process.platform !== 'linux' || process.getuid?.() === 0)('checks cwd access even when Python optimization disables assertions', async () => {
    const fake = fakeSandbox();
    const cwd = mkdtempSync(join(tmpdir(), 'dsh-prerequisites-'));
    try {
      chmodSync(cwd, 0o500);
      await createBindingOpener(fakeSdk(fake.sandbox))(options({ remoteCwd: cwd }));
      const command = fake.commands.run.mock.calls[0]?.[0];
      if (!Array.isArray(command)) throw new Error('Expected the prerequisite argv');
      // Local fixed-script validation only. Production uses the bound SDK transport.
      const check = spawnSync(command[0]!, command.slice(1), {
        cwd: '/', env: { PATH: process.env.PATH ?? '/usr/bin:/bin', PYTHONOPTIMIZE: '1' },
      });
      expect(check.error).toBeUndefined();
      expect(check.status).not.toBe(0);
    } finally {
      chmodSync(cwd, 0o700);
      rmSync(cwd, { recursive: true });
    }
  });


  it('preflights the literal dollar-expression cwd from a safe launch directory', async () => {
    const root = mkdtempSync(join(tmpdir(), 'dsh-literal-preflight-'));
    const literal = join(root, '$PROJECT'); const expansion = join(root, 'different-project');
    mkdirSync(literal); mkdirSync(expansion);
    const fake = fakeSandbox();
    try {
      const binding = await createBindingOpener(fakeSdk(fake.sandbox))(options({ remoteCwd: literal }));
      const [command, opts] = fake.commands.run.mock.calls[0]!;
      if (!Array.isArray(command)) throw new Error('Expected prerequisite argv');
      expect(opts?.workingDirectory).toBe('/'); expect(command.at(-1)).toBe(literal);
      expect(binding.descriptor.remoteCwd).toBe(literal);
      // Local fixed-script validation only, with both literal and expansion-target directories present.
      const check = spawnSync(command[0]!, command.slice(1), { cwd: '/', env: { PATH: process.env.PATH ?? '/usr/bin:/bin', PROJECT: 'different-project' } });
      expect(check.error).toBeUndefined(); expect(check.status).toBe(0);
    } finally { rmSync(root, { recursive: true }); }
  });


  it('rejects a symlink-parent cwd whose physical target is missing despite a valid logical target', async () => {
    const root = mkdtempSync(join(tmpdir(), 'dsh-physical-preflight-'));
    const logical = join(root, 'logical'); const physical = join(root, 'physical');
    mkdirSync(join(logical, 'allowed'), { recursive: true }); mkdirSync(join(physical, 'child'), { recursive: true });
    symlinkSync(join(physical, 'child'), join(logical, 'link'));
    const requested = `${logical}/link/../allowed`; const fake = fakeSandbox();
    fake.commands.run.mockImplementation(async (command, opts) => {
      if (!Array.isArray(command)) throw new Error('Expected fixed prerequisite argv');
      // Execute only the fixed prerequisite script locally at the explicit test boundary.
      const check = spawnSync(command[0]!, command.slice(1), { cwd: opts?.workingDirectory ?? '/', env: { PATH: process.env.PATH ?? '/usr/bin:/bin' } });
      return execution(check.status ?? 1);
    });
    try {
      await expect(createBindingOpener(fakeSdk(fake.sandbox))(options({ remoteCwd: requested }))).rejects.toMatchObject({ stage: 'preflight', cleanupState: 'killed' });
      const python = spawnSync('python3', ['-c', 'import os,sys; os.chdir(sys.argv[1])', requested], { cwd: '/' });
      expect(python.status).not.toBe(0); expect(fake.boundary.kill).toHaveBeenCalledOnce();
    } finally { rmSync(root, { recursive: true }); }
  });
  it('uses the same physical symlink-parent cwd as Python os.chdir', async () => {
    const root = mkdtempSync(join(tmpdir(), 'dsh-physical-preflight-'));
    const logical = join(root, 'logical'); const physical = join(root, 'physical');
    mkdirSync(join(logical, 'allowed'), { recursive: true }); mkdirSync(join(physical, 'child'), { recursive: true }); mkdirSync(join(physical, 'allowed'));
    symlinkSync(join(physical, 'child'), join(logical, 'link'));
    const requested = `${logical}/link/../allowed`; const fake = fakeSandbox();
    try {
      await createBindingOpener(fakeSdk(fake.sandbox))(options({ remoteCwd: requested }));
      const [command, opts] = fake.commands.run.mock.calls[0]!;
      if (!Array.isArray(command)) throw new Error('Expected fixed prerequisite argv');
      // A test-only pwd suffix observes the cwd chosen by the unchanged fixed prerequisite prefix.
      const inspected = [...command]; inspected[2] = `${inspected[2]}; pwd -P`;
      const check = spawnSync(inspected[0]!, inspected.slice(1), { cwd: opts?.workingDirectory ?? '/', env: { PATH: process.env.PATH ?? '/usr/bin:/bin' } });
      const python = spawnSync('python3', ['-c', 'import os,sys; os.chdir(sys.argv[1]); print(os.getcwd())', requested], { cwd: '/' });
      expect(check.status).toBe(0); expect(python.status).toBe(0);
      expect(check.stdout.toString()).toBe(python.stdout.toString());
      expect(check.stdout.toString().trim()).toBe(join(physical, 'allowed'));
    } finally { rmSync(root, { recursive: true }); }
  });

  it('requires a complete successful prerequisite execution', async () => {
    const fake = fakeSandbox();
    fake.commands.run.mockResolvedValue({ logs: { stdout: [], stderr: [] }, result: [], exitCode: 0 });
    await expect(createBindingOpener(fakeSdk(fake.sandbox))(options())).rejects.toMatchObject({ stage: 'preflight' });
  });
});

describe('public SDK transport contract', () => {
  it('forwards foreground event stream, status and interrupt using only the bound sandbox', async () => {
    const fake = fakeSandbox();
    const transport = new SdkTransport(fake.sandbox);
    const events = [];
    const signal = new AbortController().signal;
    for await (const event of transport.runStream(['bash', '-c', 'printf hello'], { workingDirectory: '/workspace', background: false }, signal)) events.push(event);
    expect(events.map(event => event.type)).toEqual(['init', 'stdout', 'stderr', 'execution_complete']);
    expect(events[0]?.text).toBe('command-a');
    await expect(transport.getCommandStatus('command-a')).resolves.toMatchObject({ running: false, exitCode: 0 });
    await transport.interrupt('command-a');
    expect(fake.commands.interrupt).toHaveBeenCalledWith('command-a');
    expect(fake.commands.runStream.mock.calls[0]?.[2]).toBe(signal);
  });

  it('preserves raw byte APIs, streaming chunks and range options', async () => {
    const fake = fakeSandbox();
    const transport = new SdkTransport(fake.sandbox);
    const bytes = new Uint8Array([0, 255, 128, 10]);
    await transport.writeBytes('/workspace/binary', bytes, { mode: 600 });
    expect(fake.files.writeFiles).toHaveBeenCalledWith([{ path: '/workspace/binary', data: bytes, mode: 600 }]);
    await expect(transport.readBytes('/workspace/binary', { offset: 1, limit: 2 })).resolves.toEqual(bytes);
    expect(fake.files.readBytes).toHaveBeenCalledWith('/workspace/binary', { offset: 1, limit: 2 });
    const chunks = [];
    for await (const chunk of transport.readBytesStream('/workspace/binary')) chunks.push(...chunk);
    expect(chunks).toEqual([...bytes]);
  });

  it('forwards command callbacks and remote request-directory operations', async () => {
    const fake = fakeSandbox();
    const transport = new SdkTransport(fake.sandbox);
    const handlers = { onInit: vi.fn() };
    await transport.run(['bash', '-c', 'true'], { background: false }, handlers);
    expect(fake.commands.run).toHaveBeenCalledWith(['bash', '-c', 'true'], { background: false }, handlers, undefined);
    await transport.createDirectories([{ path: '/workspace/private', mode: 700 }]);
    await transport.deleteFiles(['/workspace/private/input']);
    await transport.deleteDirectories(['/workspace/private']);
    expect(fake.files.createDirectories).toHaveBeenCalledWith([{ path: '/workspace/private', mode: 700 }]);
    expect(fake.files.deleteFiles).toHaveBeenCalledWith(['/workspace/private/input']);
    expect(fake.files.deleteDirectories).toHaveBeenCalledWith(['/workspace/private']);
  });
});
