// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

// Test-only transport fixtures. This plugin does not create sessions, prompt an
// Agent, execute tools, or provide any production/client service. Browser actions
// must exercise the ordinary installed CLI, Controller, Agent and tool graph.
import assert from 'node:assert/strict';
import { readFile, writeFile, rename } from 'node:fs/promises';
import { dirname, join } from 'node:path';
import { createRequire } from 'node:module';
import { pathToFileURL } from 'node:url';

export const name = 'offline-native-browser-fixture';
export const inject = ['llm', 'agentDefaultModel'];

export async function apply(ctx) {
  const root = ctx.root;
  const host = createRequire(ctx.get('profileContext').installAnchor);
  const { LlmAdapter, ToolCallId } = await import(pathToFileURL(host.resolve('@deepseek-ai/dsh-llm')).href);
  const artifact = process.env.NATIVE_TEST_ARTIFACT;
  const sdkRequire = createRequire(join(artifact, 'package.json'));
  let sdkDirectory = dirname(sdkRequire.resolve('@alibaba-group/opensandbox'));
  let manifest;
  while (dirname(sdkDirectory) !== sdkDirectory) {
    const candidate = await readFile(join(sdkDirectory, 'package.json'), 'utf8').then(JSON.parse).catch(() => undefined);
    if (candidate?.name === '@alibaba-group/opensandbox') { manifest = candidate; break; }
    sdkDirectory = dirname(sdkDirectory);
  }
  assert.equal(manifest?.version, '1.1.1');
  const { Sandbox } = await import(pathToFileURL(join(sdkDirectory, manifest.exports['.'].import)).href);
  const { helperSdk } = await import(pathToFileURL(join(process.env.NATIVE_TEST_FIXTURES, 'helper-sdk.js')).href);
  const selection = root.agentDefaultModel.currentSelection();
  const sdkCalls = [], modelRequests = [], fixtures = new Map(), originals = new Map();
  const denials = {}, errors = [];
  const originalSdk = { create: Sandbox.create, createFromTemplate: Sandbox.createFromTemplate, connect: Sandbox.connect };
  ctx.on('agent/created', ({ agent }) => {
    originals.set(agent.id, { agent, options: agent.options, session: agent.session,
      header: JSON.stringify(agent.session.header), sdkCallsAtCreate: sdkCalls.length });
  });
  const allocate = async (kind, options) => {
    assert.equal(options.skipHealthCheck, true);
    assert.equal(options.connectionConfig.domain, 'offline.invalid:443');
    const id = kind === 'connect' ? options.sandboxId : `browser-offline-${sdkCalls.length + 1}`;
    sdkCalls.push({ kind, sandboxId: id });
    const fixture = await helperSdk(id, 'browser-sdk-network-fixture');
    fixtures.set(id, fixture);
    return fixture.sandbox;
  };
  Sandbox.create = options => allocate('create-image', options);
  Sandbox.createFromTemplate = options => allocate('create-template', options);
  Sandbox.connect = options => allocate('connect', options);

  const steps = new Map();
  const script = [
    { tool: 'write', args: { file_path: 'browser-note.txt', content: 'first browser turn\n' } },
    { text: 'BROWSER_FIRST_TURN_COMPLETE: wrote browser-note.txt in the remote sandbox.' },
    { tool: 'read', args: { file_path: 'browser-note.txt' } },
    { tool: 'edit', args: { file_path: 'browser-note.txt', old_string: 'first browser turn', new_string: 'second browser turn' } },
    { tool: 'bash', args: { description: 'Read the edited remote browser note', command: 'cat browser-note.txt' } },
    { text: 'BROWSER_SECOND_TURN_COMPLETE: read, edited and checked browser-note.txt in the same remote sandbox.' },
  ];
  class ConfiguredOfflineAdapter extends LlmAdapter {
    // Unlike a controller-only probe, the ordinary GUI requires the configured
    // model to appear in the adapter's advisory catalog.
    async listModels(provider) { return [{ provider, id: selection.model, name: selection.model }]; }
    async resolveModel(provider, model) { return { provider, id: model, name: model }; }
    async *stream(options) {
      const sessionId = String(options.sessionId);
      const index = steps.get(sessionId) ?? 0;
      steps.set(sessionId, index + 1);
      const step = script[index];
      assert(step, 'Only the two documented browser prompts belong to this fixture');
      modelRequests.push({ sessionId, provider: options.provider, model: options.model,
        step: index, tool: step.tool ?? null, sdkCallsBefore: sdkCalls.length });
      if (step.tool) {
        const id = ToolCallId(`browser-offline-${modelRequests.length}`);
        const args = JSON.stringify(step.args);
        yield { type: 'block-start', index: 0, blockType: 'tool-call' };
        yield { type: 'tool-call-delta', index: 0, id, name: step.tool, argumentsDelta: args };
        yield { type: 'block-end', index: 0, block: { type: 'tool-call', id, name: step.tool, arguments: args } };
        yield { type: 'finish', reason: { kind: 'tool-calls' } };
      } else {
        yield { type: 'block-start', index: 0, blockType: 'text' };
        yield { type: 'text-delta', index: 0, text: step.text };
        yield { type: 'block-end', index: 0, block: { type: 'text', text: step.text } };
        yield { type: 'finish', reason: { kind: 'stop' } };
      }
    }
  }
  root.llm.registerAdapter([selection.provider], new ConfiguredOfflineAdapter());

  // Read-only observations plus negative host-boundary checks. None can satisfy
  // missing injection dependencies or send the browser's prompts on its behalf.
  let checkingDenials = false;
  const checkDenials = async () => {
    if (checkingDenials || !root.get('opensandboxNativeReady')) return;
    checkingDenials = true;
    const denied = async (key, operation) => {
      try { await operation(); denials[key] = false; }
      catch (error) { denials[key] = /disabled/.test(String(error)); }
    };
    await denied('hostResolve', () => root.fs.resolve('/etc/passwd'));
    denials.hostPathUnmapped = root.fs.processPathFromHostPath('/etc/passwd') === undefined;
    await denied('upload', () => root.fileUploads.upload(undefined, { name: 'must-not-upload.txt', data: 'eA==' }, new AbortController().signal));
    await denied('uploadStream', () => root.fileUploads.uploadStream({ sessionId: 'must-not-create', data: [] }));
    await denied('fileReceipt', () => root.fileUploads.bindPrompt(undefined, ['must-not-bind']));
    const text = root.fileUploads.bindPrompt(undefined, []);
    text.commit(); text[Symbol.dispose](); denials.textOnlyPromptAllowed = true;
    denials.noHostExecutionServices = ['subprocess', 'terminal', 'jobs'].every(service => !root.get(service));
  };
  let writing = false;
  const snapshot = async () => {
    if (writing) return;
    writing = true;
    try {
      await checkDenials();
      const agents = root.get('agents')?.list() ?? [];
      const sessions = agents.map(agent => {
        assert(originals.has(agent.id), 'Every exercised Agent must have been created through the real browser/controller path');
        const original = originals.get(agent.id);
        return { id: String(agent.id), header: agent.session.header,
          sameAgent: original.agent === agent, sameOptions: original.options === agent.options,
          sameSession: original.session === agent.session, sameHeader: original.header === JSON.stringify(agent.session.header),
          sdkCallsAtCreate: original.sdkCallsAtCreate,
          events: agent.session.snapshotEvents().filter(event => ['user/message', 'tool/call', 'tool/result', 'assistant/message'].includes(event.type)),
          tools: root.tools.schemas(agent).map(tool => tool.name).sort() };
      });
      const sandboxes = await Promise.all([...fixtures].map(async ([id, fixture]) => ({
        id, helperRequests: fixture.requestCount(), preflightCalls: fixture.commands.run.mock.calls.length,
        commands: fixture.calls.map(call => call.argv),
        note: await readFile(fixture.local('/workspace/browser-note.txt'), 'utf8').catch(() => null),
        closeCalls: fixture.boundary.close.mock.calls.length,
      })));
      const evidence = { fixtureReady: true, nativeReady: !!root.get('opensandboxNativeReady'),
        services: Object.fromEntries(['sessionController', 'workspaceController', 'fileUploads', 'attachments', 'fs', 'webRuntime'].map(service => [service, !!root.get(service)])),
        workspaceMetadata: {
          directory: root.get('workspaceController')?.config?.documentsDirectory ?? null,
          expectedDirectory: join(ctx.get('profileContext').dir, 'workspace-metadata'),
        },
        attachmentImageLimits: root.get('attachments')?.imageLimits ?? null,
        selection, currentSelection: root.agentDefaultModel.currentSelection(),
        sdkCalls, modelRequests, sessions, sandboxes, denials, errors };
      const target = process.env.NATIVE_BROWSER_EVIDENCE;
      await writeFile(`${target}.tmp`, JSON.stringify(evidence, null, 2));
      await rename(`${target}.tmp`, target);
    } catch (error) {
      errors.push(String(error));
      console.error('NATIVE_BROWSER_FIXTURE_ERROR', error);
    } finally { writing = false; }
  };
  const interval = setInterval(() => { void snapshot(); }, 200);
  await snapshot();
  console.log('NATIVE_BROWSER_FIXTURE_READY transport-only no-session-or-prompt-injection');
  ctx.effect(() => async () => {
    clearInterval(interval);
    Object.assign(Sandbox, originalSdk);
    await Promise.allSettled([...fixtures.values()].map(fixture => fixture.cleanup()));
  });
}
