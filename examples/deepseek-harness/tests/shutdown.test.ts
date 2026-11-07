// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it, vi } from 'vitest';
import { LlmAdapter } from '@deepseek-ai/dsh-llm';
import type { GenerateOptions, LlmResolvedModelInfo, StreamChunk } from '@deepseek-ai/dsh-llm';
import type { BoundSandbox, HeadlessSession } from '@opensandbox/deepseek-harness';
import { createContractTransport } from '../contract-transport.js';
import { main } from '../headless.js';

const mocks = vi.hoisted(() => ({openBinding: vi.fn(), createHeadlessSession: vi.fn()}));
vi.mock('@opensandbox/deepseek-harness', async importOriginal => {
  const actual = await importOriginal<typeof import('@opensandbox/deepseek-harness')>();
  return {...actual, ...mocks};
});
function gate() {
  let resolve!: () => void;
  const promise = new Promise<void>(done => {resolve = done;});
  return {promise, resolve};
}
class LocalAdapter extends LlmAdapter {
  calls = 0;
  override async resolveModel(provider: string, model: string): Promise<LlmResolvedModelInfo> {return {provider, id: model, name: model};}
  async *stream(_options: GenerateOptions): AsyncIterable<StreamChunk> {
    this.calls++;
    yield {type: 'block-start', index: 0, blockType: 'text'};
    yield {type: 'text-delta', index: 0, text: 'local contract complete'};
    yield {type: 'block-end', index: 0, block: {type: 'text', text: 'local contract complete'}};
    yield {type: 'finish', reason: {kind: 'stop'}};
  }
}

describe('signals during actual Agent shutdown', () => {
  it('contains repeated SIGINT/SIGTERM after disposal while explicit kill and close are pending', async () => {
    const transport = await createContractTransport();
    const actual = await vi.importActual<typeof import('@opensandbox/deepseek-harness')>('@opensandbox/deepseek-harness');
    const killStarted = gate(), allowKill = gate(), closeStarted = gate(), allowClose = gate();
    const phases: string[] = [], adapter = new LocalAdapter();
    let owned: HeadlessSession | undefined;
    const binding: BoundSandbox = {...transport.binding,
      kill: async () => {phases.push('kill'); killStarted.resolve(); await allowKill.promise;},
      close: async () => {phases.push('close'); closeStarted.resolve(); await allowClose.promise; await transport.binding.close();},
    };
    mocks.openBinding.mockResolvedValue(binding);
    mocks.createHeadlessSession.mockImplementation(async (options: Parameters<typeof actual.createHeadlessSession>[0]) => {
      owned = await actual.createHeadlessSession({...options, adapter});
      return {...owned, dispose: async () => {phases.push('dispose'); await owned!.dispose();}};
    });
    const env = {DSH_REAL_MODEL: '1', OPEN_SANDBOX_DOMAIN: 'example.invalid', OPEN_SANDBOX_PROTOCOL: 'https',
      DSH_SESSION_ID: 'contract-only-session', DSH_REMOTE_CWD: '/workspace', DSH_SANDBOX_MODE: 'connect',
      DSH_SANDBOX_ID: 'contract-only-sandbox', DSH_CLEANUP: 'kill', DEEPSEEK_API_KEY: 'fake-key-never-sent',
      DEEPSEEK_BASE_URL: 'https://example.invalid/anthropic', DSH_MODEL: 'scripted-local',
      DSH_USER_ID: '8aa78e38-c47a-4b19-a805-c84d276ef106', DSH_PROMPT: 'Run a local scripted contract.'};
    for (const [key, value] of Object.entries(env)) vi.stubEnv(key, value);
    const fetch = vi.spyOn(globalThis, 'fetch').mockRejectedValue(new Error('Network forbidden'));
    const log = vi.spyOn(console, 'log').mockImplementation(() => undefined);
    const errors = vi.spyOn(console, 'error').mockImplementation(() => undefined);
    const previousExit = process.exitCode;
    process.exitCode = 0;
    const pending = main();
    try {
      await killStarted.promise;
      expect(owned).toBeDefined();
      expect(() => owned!.agent.cancel({kind: 'user'})).toThrow(/projection registration is not active/);
      for (const event of ['SIGINT', 'SIGTERM', 'SIGINT', 'SIGTERM'] as const) expect(() => process.emit(event)).not.toThrow();
      expect(phases).toEqual(['dispose', 'kill']);
      allowKill.resolve(); await closeStarted.promise;
      for (const event of ['SIGTERM', 'SIGINT', 'SIGTERM', 'SIGINT'] as const) expect(() => process.emit(event)).not.toThrow();
      expect(phases).toEqual(['dispose', 'kill', 'close']);
      allowClose.resolve(); await pending;
      expect(log.mock.calls.map(([line]) => JSON.parse(line as string))).toContainEqual({mode: 'real', cleanup: {disposed: true, deleted: true, closed: true}});
      expect(JSON.stringify(log.mock.calls)).not.toContain('fake-key-never-sent');
      expect(errors).not.toHaveBeenCalled(); expect(fetch).not.toHaveBeenCalled();
      expect(adapter.calls).toBe(1); expect(process.exitCode).toBe(0);
    } finally {
      allowKill.resolve(); allowClose.resolve(); await pending.catch(() => undefined);
      await owned?.dispose(); await transport.binding.close(); await transport.cleanup();
      process.exitCode = previousExit; vi.unstubAllEnvs(); vi.restoreAllMocks(); mocks.openBinding.mockReset(); mocks.createHeadlessSession.mockReset();
    }
  }, 30_000);
});
