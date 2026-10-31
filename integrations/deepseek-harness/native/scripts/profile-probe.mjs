// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

// Offline acceptance of the *installed default entry*, never a replacement native plugin.
// Only the installed SDK's static network boundary and configured LLM transport are fixtures.
// The real CLI Web profile, SessionController, AgentLoop, binding opener, stock tools,
// remote providers and packaged Python helper remain in the exercised call path.
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { dirname, join } from 'node:path';
import { createRequire } from 'node:module';
import { pathToFileURL } from 'node:url';

export const inject = ['opensandboxNativeReady', 'agentPresets', 'sessionController', 'llm', 'agentDefaultModel', 'tools', 'fs', 'agents', 'sessions'];
export const name = 'offline-native-profile-probe';
export function apply(ctx) {
  const timer = setTimeout(async () => {
    const fixtures = [];
    let restoreSdk = () => {};
    try {
      const root = ctx.root;
      const host = createRequire(ctx.get('profileContext').installAnchor);
      const module = async specifier => import(pathToFileURL(host.resolve(specifier)).href);
      const { Context } = await module('@deepseek-ai/cordis');
      const { ShellExecutor } = await module('@deepseek-ai/dsh-shell');
      const { FileSystem, FsError } = await module('@deepseek-ai/dsh-fs');
      const { LlmAdapter, ToolCallId } = await module('@deepseek-ai/dsh-llm');
      const artifact = process.env.NATIVE_TEST_ARTIFACT;
      const sdkRequire = createRequire(`${artifact}/package.json`);
      // require.resolve selects the SDK's CJS conditional export. Follow its package
      // manifest to the ESM export used by the installed core binding, not a second class.
      let sdkDirectory = dirname(sdkRequire.resolve('@alibaba-group/opensandbox'));
      let sdkManifest;
      while (dirname(sdkDirectory) !== sdkDirectory) {
        const candidate = await readFile(join(sdkDirectory, 'package.json'), 'utf8').then(JSON.parse).catch(() => undefined);
        if (candidate?.name === '@alibaba-group/opensandbox') { sdkManifest = candidate; break; }
        sdkDirectory = dirname(sdkDirectory);
      }
      assert.equal(sdkManifest?.version, '1.1.1');
      assert.equal(typeof sdkManifest.exports['.'].import, 'string');
      const { Sandbox, SandboxApiException, SandboxError } = await import(pathToFileURL(join(sdkDirectory, sdkManifest.exports['.'].import)).href);
      const { helperSdk } = await import(pathToFileURL(`${process.env.NATIVE_TEST_FIXTURES}/helper-sdk.js`).href);
      const { RemoteShell } = await import(pathToFileURL(`${artifact}/dist/core/shell.js`).href);
      const { RemoteFileSystem } = await import(pathToFileURL(`${artifact}/dist/core/filesystem.js`).href);
      const native = await import(pathToFileURL(`${artifact}/dist/index.js`).href);
      assert.equal(Object.getPrototypeOf(RemoteShell.prototype).constructor, ShellExecutor);
      assert.equal(Object.getPrototypeOf(RemoteFileSystem.prototype).constructor, FileSystem);
      assert(root instanceof Context);
      assert.equal(root.opensandboxNativeReady.preset, 'opensandbox');
      assert.equal((await root.agentPresets.resolve()).id, 'opensandbox');
      assert.deepEqual((await root.agentPresets.list()).map(preset => preset.id), ['opensandbox']);

      const sdkCalls = [];
      const byId = new Map();
      const originalSdk = { create: Sandbox.create, createFromTemplate: Sandbox.createFromTemplate, connect: Sandbox.connect };
      let nextFailure;
      let counter = 0;
      const allocated = async (kind, options) => {
        const failure = nextFailure; nextFailure = undefined;
        sdkCalls.push({ kind, sandboxId: options.sandboxId, failure });
        assert.equal(options.skipHealthCheck, true, 'The unchanged binding opener must own readiness');
        assert.equal(options.connectionConfig.domain, process.env.OPENSANDBOX_DOMAIN);
        if (failure === 'bad-address') throw new Error('private-wrong-address.invalid private-api-key');
        if (failure === 'bad-key') throw new SandboxApiException({ message: 'private-api-key', statusCode: 401,
          error: new SandboxError('FSB::API_ERROR', 'private-api-key'), rawBody: { endpoint: 'private-wrong-address.invalid' }, requestId: 'private-request' });
        const id = kind === 'connect' ? options.sandboxId : `native-offline-${++counter}`;
        const sdk = await helperSdk(id, 'sdk-network-fixture'); fixtures.push(sdk); byId.set(id, sdk);
        if (failure === 'missing-capability') sdk.commands.run.mockResolvedValue({ id: 'preflight-denied', logs: { stdout: [], stderr: [] }, result: [], complete: { timestamp: 1, executionTimeMs: 1 }, exitCode: 127 });
        return sdk.sandbox;
      };
      Sandbox.create = options => allocated('create-image', options);
      Sandbox.createFromTemplate = options => allocated('create-template', options);
      Sandbox.connect = options => allocated('connect', options);
      restoreSdk = () => { Object.assign(Sandbox, originalSdk); };

      const before = root.agentDefaultModel.currentSelection();
      const modelRequests = [], scripts = new Map();
      class ExistingConfiguredAdapter extends LlmAdapter {
        async resolveModel(provider, model) { return { provider, id: model, name: model }; }
        async *stream(options) {
          modelRequests.push({ provider: options.provider, model: options.model, sessionId: options.sessionId });
          const step = scripts.get(String(options.sessionId))?.shift() ?? { text: 'offline native model completed' };
          if (step.tool) {
            const id = ToolCallId(`offline-model-${modelRequests.length}`), args = JSON.stringify(step.args);
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
      root.llm.registerAdapter([before.provider], new ExistingConfiguredAdapter());
      let callId = 0;
      const tool = (agent, name, args) => root.tools.execute({ callId: ToolCallId(`profile-${++callId}`), name, arguments: args, ...(agent ? { agent } : {}), signal: new AbortController().signal });
      const resolve = async sessionId => {
        const result = await root.sessionController.resolveAgent(sessionId);
        if ('error' in result) throw result.error;
        return result.agent;
      };
      const create = async sessionId => {
        const count = sdkCalls.length;
        const created = await root.sessionController.create({ sessionId, cwd: process.env.NATIVE_TEST_CWD, agentPreset: 'opensandbox' });
        assert.equal(created.agentPreset, 'opensandbox');
        const agent = await resolve(sessionId);
        assert.equal(sdkCalls.length, count, 'Native session creation must remain network-free');
        return agent;
      };
      const success = result => { assert.equal(result.isError, false, JSON.stringify(result)); return result; };
      const status = async agent => success(await tool(agent, 'opensandbox_status', {})).value;
      const bash = command => ({ description: 'Offline installed native command', command });
      const id = 'native-install-session';
      const mode = process.env.NATIVE_TEST_MODE;
      const agent = mode === 'create' ? await create(id) : await resolve(id);
      const options = agent.options, header = structuredClone(agent.session.header);
      assert.deepEqual(root.agentDefaultModel.currentSelection(), before);
      assert.equal(options.provider, before.provider); assert.equal(options.model, before.model);
      assert.equal(header.cwd, process.env.NATIVE_TEST_CWD); assert.equal(header.agentPreset, 'opensandbox');
      assert.deepEqual(root.tools.schemas(agent).map(row => row.name).sort(),
        ['bash', 'read', 'write', 'edit', 'opensandbox_status', 'opensandbox_reconnect', 'opensandbox_delete'].sort());
      assert.equal((await status(agent)).state, mode === 'create' ? 'unopened' : 'reconnect-required');
      assert.equal((await tool(agent, 'opensandbox_delete', { confirm_sandbox_id: 'not-created' })).isError, true);
      assert.equal(sdkCalls.length, 0, 'Enable, create/resume, status and idle delete must not acquire');
      assert.equal((await tool(undefined, 'bash', bash('false'))).isError, true);
      assert.equal((await tool(agent, 'run_code', { code: 'throw new Error("unreachable")' })).isError, true);
      await assert.rejects(root.fs.resolve('/etc/passwd'), /disabled/);
      assert.equal(root.fs.processPathFromHostPath('/etc/passwd'), undefined);
      await assert.rejects(root.sessionController.openWorkspacePath({ path: '/etc/passwd' }, new AbortController().signal));
      assert.equal(root.get('subprocess'), undefined);
      assert.equal(root.get('terminal'), undefined);
      assert.equal(root.get('jobs'), undefined);
      let hostToolCalls = 0;
      agent.ctx.tools.register({ name: 'unapproved_host_tool', description: 'Must never execute', parameters: { type: 'object', properties: {} },
        output: { schema: { type: 'string' }, render: (_, value) => [{ type: 'text', text: String(value) }] },
        async execute() { hostToolCalls++; return 'host'; } });
      assert.equal((await tool(agent, 'unapproved_host_tool', {})).isError, true);
      if (mode === 'resume') {
        assert.equal((await tool(agent, 'read', { file_path: 'note.txt' })).isError, true);
        assert.equal(sdkCalls.length, 0, 'Cold resume must not create despite persisted native conversation');
        success(await tool(agent, 'opensandbox_reconnect', { sandbox_id: 'native-offline-1' }));
        assert.deepEqual(sdkCalls.map(call => call.kind), ['connect']);
      }

      // Two turns drive the original native Agent/model, not a headless wrapper or another Agent.
      scripts.set(id, [{ tool: 'bash', args: bash('printf first > note.txt') }, { text: 'first turn complete' }]);
      await root.sessionController.prompt({ sessionId: id, requestId: `${mode}-first`, mode: 'queue', content: [{ type: 'text', text: 'Create the remote note.' }] }, new AbortController().signal);
      await agent.whenIdle();
      const first = await status(agent), bindingId = first.sandboxId;
      assert.equal(first.state, 'open'); assert(bindingId);
      assert.equal(sdkCalls.length, 1);
      assert.equal(await readFile(byId.get(bindingId).local('/workspace/note.txt'), 'utf8'), 'first');
      scripts.set(id, [{ tool: 'read', args: { file_path: 'note.txt' } },
        { tool: 'edit', args: { file_path: 'note.txt', old_string: 'first', new_string: 'second' } }, { text: 'second turn complete' }]);
      await root.sessionController.prompt({ sessionId: id, requestId: `${mode}-second`, mode: 'queue', content: [{ type: 'text', text: 'Read and edit the existing remote note.' }] }, new AbortController().signal);
      await agent.whenIdle();
      assert.equal((await status(agent)).sandboxId, bindingId);
      assert.equal(sdkCalls.length, 1);
      assert.equal(await readFile(byId.get(bindingId).local('/workspace/note.txt'), 'utf8'), 'second');
      assert.equal(root.agents.get(agent.id), agent); assert.equal(agent.options, options); assert.deepEqual(agent.session.header, header);
      assert.equal(modelRequests.length, 5);
      assert(modelRequests.every(request => request.provider === before.provider && request.model === before.model && String(request.sessionId) === id));
      assert(agent.session.snapshotEvents().filter(event => event.type === 'tool/result').every(event => !event.data.message.isError));
      await root.sessions.flush(agent.session);

      if (mode === 'create') {
        const fileFirst = await create('native-file-first');
        success(await tool(fileFirst, 'write', { file_path: 'note.txt', content: 'separate-session' }));
        const fileId = (await status(fileFirst)).sandboxId;
        assert.notEqual(fileId, bindingId);
        const concurrent = await create('native-concurrent');
        const beforeConcurrent = sdkCalls.length;
        const results = await Promise.all([tool(concurrent, 'write', { file_path: 'note.txt', content: 'concurrent' }), tool(concurrent, 'bash', bash('printf concurrent-shell'))]);
        results.forEach(success);
        assert.equal(sdkCalls.length, beforeConcurrent + 1, 'Concurrent first shell/file must share one SDK creation');
        assert.equal(await readFile(byId.get(fileId).local('/workspace/note.txt'), 'utf8'), 'separate-session');
        assert.equal(await readFile(byId.get(bindingId).local('/workspace/note.txt'), 'utf8'), 'second');
        success(await tool(fileFirst, 'read', { file_path: 'note.txt' }));
        assert.equal((await tool(fileFirst, 'opensandbox_delete', { confirm_sandbox_id: bindingId })).isError, true);
        success(await tool(fileFirst, 'opensandbox_delete', { confirm_sandbox_id: fileId }));
        const afterDelete = sdkCalls.length;
        assert.equal((await tool(fileFirst, 'bash', bash('printf no-replacement'))).isError, true);
        assert.equal(sdkCalls.length, afterDelete);

        for (const failure of ['bad-address', 'bad-key', 'missing-capability']) {
          const failed = await create(`native-${failure}`), count = sdkCalls.length;
          nextFailure = failure;
          const failedResults = await Promise.all([tool(failed, 'bash', bash('printf no-host-fallback')), tool(failed, 'read', { file_path: 'note.txt' })]);
          failedResults.push(await tool(failed, 'bash', bash('printf no-retry')));
          assert(failedResults.every(result => result.isError));
          assert.equal(sdkCalls.length, count + 1);
          assert.doesNotMatch(JSON.stringify(failedResults), /private-api-key|private-wrong-address|private-request/);
          const facts = await status(failed);
          assert.equal(facts.state, failure === 'missing-capability' ? 'killed' : 'outcome-unknown');
          if (failure === 'bad-key') assert.equal(facts.statusCode, 401);
        }
      }
      assert.equal(hostToolCalls, 0);
      assert(fixtures.every(sdk => sdk.commands.run.mock.calls.length >= 1), 'Installed binding opener must invoke SDK preflight');
      assert(fixtures.some(sdk => sdk.requestCount() > 0), 'Actual packaged Python helper must execute');
      const identity = new Context();
      try {
        // Reject before any SDK operation, using the actual packed provider and
        // host error constructor. This scope does not own/close the borrowed SDK.
        await identity.plugin(RemoteFileSystem, { binding: byId.get(bindingId).binding });
        const foreign = { targetKey: 'foreign', displayPath: '/foreign' };
        await assert.rejects(identity.fs.readText(foreign), error => error instanceof FsError && error.code === 'FS_IO_ERROR');
        await assert.rejects(identity.fs.writeText(foreign, 'x', undefined, undefined, { mode: 'read-only' }),
          error => error instanceof FsError && error.code === 'FS_SANDBOX_DENIED');
      } finally { await identity.fiber.dispose(); }

      // Public native registry teardown verifies provider drain and independent SDK-local close.
      // It also removes readiness, so the persistent profile gate drains native loop/controller.
      const runtime = root.registry.get(native.default);
      assert(runtime, 'The installed default plugin, not a replacement test plugin, must own readiness');
      await Promise.all([...runtime.fibers].map(fiber => fiber.dispose()));
      for (const sdk of fixtures) {
        assert.equal(sdk.boundary.close.mock.calls.length, 1, 'Every allocated SDK transport must close once');
        assert.deepEqual(await sdk.artifactRoots(), [], 'Provider drain must remove helper staging files');
      }
      assert.equal((await tool(agent, 'bash', bash('printf no-host-after-unload'))).isError, true);
      assert.equal(hostToolCalls, 0);
      restoreSdk();
      await Promise.all(fixtures.map(sdk => sdk.cleanup()));
      console.log(`NATIVE_PROFILE_PASS ${mode} installed-default-entry actual-controller native-tools two-model-turns sdk-singleflight helper peer-identities fail-closed cleanup zero-network`);
      process.emit('SIGTERM');
    } catch (error) {
      restoreSdk();
      await Promise.allSettled(fixtures.map(sdk => sdk.cleanup()));
      console.error('NATIVE_PROFILE_FAILED', error);
      process.exitCode = 1;
      process.emit('SIGTERM');
    }
  }, 100);
  ctx.effect(() => () => clearTimeout(timer));
}
