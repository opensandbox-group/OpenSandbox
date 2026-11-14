// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

import { LlmAdapter } from '@deepseek-ai/dsh-llm';
import type { GenerateOptions, LlmResolvedModelInfo, StreamChunk } from '@deepseek-ai/dsh-llm';
import { createHeadlessSession } from '@opensandbox/deepseek-harness';
import { describe, expect, it, vi } from 'vitest';
import { createContractTransport } from '../contract-transport.js';
import { cleanupSession } from '../headless.js';

function gate() {
  let resolve!: () => void;
  const promise = new Promise<void>(done => { resolve = done; });
  return { promise, resolve };
}
class UnusedAdapter extends LlmAdapter {
  override async resolveModel(provider: string, model: string): Promise<LlmResolvedModelInfo> { return { provider, id: model, name: model }; }
  async *stream(_options: GenerateOptions): AsyncIterable<StreamChunk> { throw new Error('No model calls are expected.'); }
}

describe('installed filesystem provider cleanup evidence', () => {
  it('reports disposed:false for an unconfirmed helper while still attempting explicit kill and close', async () => {
    const transport = await createContractTransport(), started = gate(), endStream = gate();
    const phases: string[] = [];
    const binding = { ...transport.binding,
      get state() { return transport.binding.state; },
      kill: async () => { phases.push('kill'); await transport.binding.kill(); },
      close: async () => { phases.push('close'); await transport.binding.close(); },
    };
    const session = await createHeadlessSession({ binding, adapter: new UnusedAdapter(), provider: 'offline', model: 'unused' });
    const fs = session.agent.ctx.root.fs, target = await fs.resolve('note.txt');
    const commands = binding.sandbox.commands, original = commands.runStream.bind(commands);
    const run = vi.spyOn(commands, 'runStream').mockImplementation(async function* (input, options) {
      if (Array.isArray(input) && input[1]?.endsWith('/fs_helper.py')) {
        yield { type: 'init', text: 'unconfirmed-fs-helper' };
        started.resolve(); await endStream.promise; return;
      }
      yield* original(input, options);
    });
    const interrupt = vi.spyOn(commands, 'interrupt').mockRejectedValue(new Error('private-interrupt-diagnostic'));
    vi.spyOn(commands, 'getCommandStatus').mockResolvedValue({ id: 'unconfirmed-fs-helper', running: true, exitCode: null });
    const pending = fs.writeText(target, 'must not claim clean disposal').catch(error => error);
    try {
      await started.promise; vi.useFakeTimers();
      const cleaning = cleanupSession(session, binding, 'kill');
      await vi.advanceTimersByTimeAsync(6_100);
      expect(await cleaning).toEqual({ disposed: false, deleted: true, closed: true });
      expect(await pending).toMatchObject({ code: 'FS_IO_ERROR', outcome: 'unknown', remoteState: 'unknown' });
      expect(JSON.stringify(await pending)).not.toContain('private-interrupt-diagnostic');
      expect(phases).toEqual(['kill', 'close']);
      expect(interrupt).toHaveBeenCalledExactlyOnceWith('unconfirmed-fs-helper');
      expect(run.mock.calls.filter(([input]) => Array.isArray(input) && input[1]?.endsWith('/fs_helper.py'))).toHaveLength(1);
      expect(session.agent.ctx.root.registry.size).toBe(0);
    } finally {
      endStream.resolve(); vi.useRealTimers();
      await pending; await session.dispose().catch(() => undefined);
      vi.restoreAllMocks(); await transport.binding.close(); await transport.cleanup();
    }
  }, 15_000);
});
