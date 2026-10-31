// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it, vi } from 'vitest';

import { spawnSync } from 'node:child_process';
import { readConfig, summarizeRun, toolFailed, cleanupSession, createModelAdapter } from '../headless.js';
const env = {
  DSH_REAL_MODEL: '1', OPEN_SANDBOX_DOMAIN: 'example.invalid:8080', OPEN_SANDBOX_PROTOCOL: 'http',
  DSH_SESSION_ID: 'explicit-session', DSH_REMOTE_CWD: '/workspace', DSH_SANDBOX_MODE: 'create-image',
  DSH_IMAGE: 'python:3.12', DSH_TTL_SECONDS: '600', DSH_CLEANUP: 'close',
  DEEPSEEK_API_KEY: 'test-key-never-sent', DEEPSEEK_BASE_URL: 'https://example.invalid/anthropic',
  DSH_MODEL: 'explicit-model', DSH_USER_ID: '8aa78e38-c47a-4b19-a805-c84d276ef106', DSH_PROMPT: 'Review a test file.',
};
describe('explicit real headless configuration and terminal evidence', () => {
  it('exits nonzero before configuration or requests when the CLI opt-in is absent', () => {
    const result = spawnSync(process.execPath, ['--import', 'tsx', 'headless.ts'], {cwd: new URL('../', import.meta.url), env: {PATH: process.env.PATH}, encoding: 'utf8', timeout: 10_000});
    expect(result.status).toBe(1);
    expect(result.stderr).toContain('DSH_REAL_MODEL=1');
    expect(result.stdout).toBe('');
  });
  it('requires opt-in and every required value before any model or service call', async () => {
    expect(() => readConfig({})).toThrow('DSH_REAL_MODEL');
    for (const key of Object.keys(env).filter(key => key !== 'DSH_REAL_MODEL')) {
      const candidate = { ...env, [key]: undefined };
      expect(() => readConfig(candidate), key).toThrow();
    }
    expect(() => readConfig({ ...env, DSH_TTL_SECONDS: '0' })).toThrow();
    expect(() => readConfig({ ...env, DSH_CLEANUP: 'automatic' })).toThrow();
    expect(() => readConfig({ ...env, DEEPSEEK_BASE_URL: 'http://example.invalid' })).toThrow();
    expect(() => readConfig({ ...env, DEEPSEEK_BASE_URL: 'https://user:private@example.invalid' })).toThrow();
  });
  it('maps explicit SDK image/template/connect options without silently creating on reconnect failure', async () => {
    expect(readConfig(env).openOptions).toMatchObject({kind: 'create-image', image: 'python:3.12', timeoutSeconds: 600});
    expect(readConfig({ ...env, DSH_SANDBOX_MODE: 'create-template', DSH_TEMPLATE_ID: 'template-1' }).openOptions)
      .toMatchObject({kind: 'create-template', templateId: 'template-1', timeoutSeconds: 600});
    expect(readConfig({ ...env, DSH_SANDBOX_MODE: 'connect', DSH_SANDBOX_ID: 'live-1' }).openOptions)
      .toMatchObject({kind: 'connect', sandboxId: 'live-1'});
    expect(() => readConfig({ ...env, DSH_SANDBOX_MODE: 'connect' })).toThrow('DSH_SANDBOX_ID');
  });
  it('checks durable turn/tool outcomes even when run resolves and ignores an earlier successful turn', async () => {
    const events = [ {type: 'turn/end', data: {reason: {kind: 'completed'}}},
      {type: 'tool/result', data: {message: {isError: true}}}, {type: 'turn/end', data: {reason: {kind: 'error'}}} ];
    const agent = {session: {snapshotEvents: () => events}} as unknown as Parameters<typeof summarizeRun>[0];
    expect(summarizeRun(agent, 1, 0, 0)).toMatchObject({success: false, reason: 'error', toolErrors: 1});
    expect(summarizeRun(agent, 3, 0, 0)).toMatchObject({success: false, reason: 'missing'});
    expect(summarizeRun({session: {snapshotEvents: () => [{type: 'turn/end', data: {reason: {kind: 'completed'}}}]}} as unknown as Parameters<typeof summarizeRun>[0], 0, 1, 0).success).toBe(false);
    expect(toolFailed({kind: 'foreground', exitCode: 1, timedOut: false})).toBe(true);
    expect(toolFailed({kind: 'foreground', exitCode: 0, timedOut: true})).toBe(true);
    expect(toolFailed({kind: 'foreground', exitCode: 0, timedOut: false})).toBe(false);
  });
  it('attempts dispose, explicit kill and close independently and reports unconfirmed cleanup', async () => {
    const calls: string[] = [];
    const binding = {kill: async () => { calls.push('kill'); throw new Error('do-not-print'); }, close: async () => {calls.push('close');}};
    const session = {dispose: async () => {calls.push('dispose'); throw new Error('do-not-print');}};
    expect(await cleanupSession(session, binding, 'kill')).toEqual({disposed: false, deleted: false, closed: true});
    expect(calls).toEqual(['dispose', 'kill', 'close']);
    calls.length = 0;
    expect(await cleanupSession(undefined, binding, 'close')).toEqual({disposed: true, deleted: null, closed: true});
    expect(calls).toEqual(['close']);
  });
  it('constructs the official adapter without requests, ambient identity files or credential logs', async () => {
    const fetch = vi.spyOn(globalThis, 'fetch').mockRejectedValue(new Error('network forbidden in this test'));
    try {
      const adapter = createModelAdapter(readConfig(env));
      expect(adapter.constructor.name).toBe('DeepSeekAdapter');
      expect(await adapter.resolveModel('deepseek-official', env.DSH_MODEL)).toMatchObject({provider: 'deepseek-official', id: env.DSH_MODEL});
      expect(fetch).not.toHaveBeenCalled();
    } finally {fetch.mockRestore();}
  });
});
