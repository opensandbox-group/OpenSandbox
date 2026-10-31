// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

import assert from 'node:assert/strict';
import { pathToFileURL } from 'node:url';
import type {} from '@deepseek-ai/dsh-tools';
import { LlmAdapter, ToolCallId } from '@deepseek-ai/dsh-llm';
import type { GenerateOptions, LlmResolvedModelInfo, StreamChunk } from '@deepseek-ai/dsh-llm';
import { SdkTransport, createHeadlessSession } from '@opensandbox/deepseek-harness';
import type { HeadlessSession } from '@opensandbox/deepseek-harness';
import { createContractTransport, SMOKE_COMMAND } from './contract-transport.js';
import { summarizeRun, toolFailed } from './outcome.js';

function tool(id: string, name: string, args: object): StreamChunk[] {
  const callId = ToolCallId(id), argumentsJson = JSON.stringify(args);
  return [{type: 'block-start', index: 0, blockType: 'tool-call'},
    {type: 'tool-call-delta', index: 0, id: callId, name, argumentsDelta: argumentsJson},
    {type: 'block-end', index: 0, block: {type: 'tool-call', id: callId, name, arguments: argumentsJson}},
    {type: 'finish', reason: {kind: 'tool-calls'}}];
}
class ScriptedAdapter extends LlmAdapter {
  readonly script = [
    tool('contract-bash', 'bash', {description: 'Write the fixed contract file', command: SMOKE_COMMAND}),
    tool('contract-read', 'read', {file_path: 'note.txt'}),
    tool('contract-edit', 'edit', {file_path: 'note.txt', old_string: 'contract', new_string: 'contract-edited'}),
    [{type: 'block-start', index: 0, blockType: 'text'}, {type: 'text-delta', index: 0, text: 'done'},
      {type: 'block-end', index: 0, block: {type: 'text', text: 'done'}}, {type: 'finish', reason: {kind: 'stop'}}] satisfies StreamChunk[],
  ];
  override async resolveModel(provider: string, model: string): Promise<LlmResolvedModelInfo> {return {provider, id: model, name: model};}
  async *stream(_options: GenerateOptions): AsyncIterable<StreamChunk> {
    const response = this.script.shift(); assert(response, 'Contract script exhausted');
    yield* response;
  }
}
export async function smoke() {
  const transport = await createContractTransport();
  let session: HeadlessSession | undefined;
  try {
    session = await createHeadlessSession({binding: transport.binding, adapter: new ScriptedAdapter(), provider: 'contract-only', model: 'scripted-no-network'});
    let errors = 0, failures = 0;
    const tools: string[] = [];
    session.agent.ctx.on('agent/error', () => {errors++;});
    session.agent.ctx.on('tools/result', (exec, result) => {tools.push(exec.name); if (!result.isError && toolFailed(result.value)) failures++;});
    const offset = session.agent.session.snapshotEvents().length;
    await session.run('Run the fixed offline contract script.');
    const outcome = summarizeRun(session.agent, offset, errors, failures);
    assert(outcome.success, 'Quiescent contract turn failed');
    assert.deepEqual(tools, ['bash', 'read', 'edit']);
    assert.equal(await transport.readText(), 'contract-edited');
    const bytes = Uint8Array.from([0, 255, 128, 10, 0xc3, 0x28, 0, 1]);
    const sdk = new SdkTransport(transport.binding.sandbox);
    await sdk.writeBytes('/workspace/binary.bin', bytes, {mode: 600});
    assert.deepEqual(await sdk.readBytes('/workspace/binary.bin'), bytes);
    const streamed: number[] = [];
    for await (const chunk of sdk.readBytesStream('/workspace/binary.bin')) streamed.push(...chunk);
    assert.deepEqual(Uint8Array.from(streamed), bytes);
    return {mode: 'contract-only', deployed: false, modelCalls: 0, tools, binaryBytes: bytes.length, outcome};
  } finally {
    try { await session?.dispose(); } finally {await transport.binding.close(); await transport.cleanup();}
  }
}
if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  console.log('Offline contract-only smoke. Local disposable simulation; no deployed sandbox or paid model.');
  await smoke().then(result => console.log(JSON.stringify(result))).catch(() => {
    process.exitCode = 1; console.error('Offline contract-only smoke failed.');
  });
}
