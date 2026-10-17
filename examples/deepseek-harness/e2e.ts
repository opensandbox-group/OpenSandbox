// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

import assert from 'node:assert/strict';
import { createHash, randomUUID } from 'node:crypto';
import { accessSync, constants, lstatSync, closeSync, openSync, realpathSync, writeFileSync } from 'node:fs';
import { basename, dirname, isAbsolute, join, relative, sep } from 'node:path';
import { pathToFileURL } from 'node:url';
import type {} from '@deepseek-ai/dsh-tools';
import { LlmAdapter, ToolCallId } from '@deepseek-ai/dsh-llm';
import type { GenerateOptions, LlmResolvedModelInfo, StreamChunk } from '@deepseek-ai/dsh-llm';
import type { CommandStatus } from '@alibaba-group/opensandbox';
import { Sandbox, SandboxManager, SandboxApiException } from '@alibaba-group/opensandbox';
import { SdkTransport, createHeadlessSession, createBindingOpener, BindingOpenError, UnknownOutcomeError, RemoteShellError } from '@opensandbox/deepseek-harness';
import type { BoundSandbox, HeadlessSession } from '@opensandbox/deepseek-harness';
import { summarizeRun, toolFailed } from './outcome.js';

class ConfigError extends Error {}
export interface E2eConfig {
  domain: string; protocol: 'http' | 'https'; apiKey: string; templateId: string;
  sessionId: string; remoteCwd: string; timeoutSeconds: number;
  work: string; evidencePath: string; downloadsDir: string; hostObservationDir: string;
}
function inside(path: string, parent: string): boolean {
  const rel = relative(parent, path); return rel === '' || (!rel.startsWith(`..${sep}`) && rel !== '..' && !isAbsolute(rel));
}
export function readE2eConfig(env: NodeJS.ProcessEnv = process.env): E2eConfig {
  const required = (key: string) => {
    const value = env[key];
    if (!value?.trim() || /[\x00-\x1f\x7f]/.test(value)) throw new ConfigError(`${key} must be explicitly set without control characters.`);
    return value;
  };
  if (env.DSH_LIVE_E2E !== '1') throw new ConfigError('Set DSH_LIVE_E2E=1 only to create, modify and delete two real template sandboxes.');
  const domain = required('OPEN_SANDBOX_DOMAIN'), protocol = required('OPEN_SANDBOX_PROTOCOL');
  if (protocol !== 'http' && protocol !== 'https') throw new ConfigError('OPEN_SANDBOX_PROTOCOL must be http or https.');
  let endpoint: URL;
  try { endpoint = new URL(`${protocol}://${domain}`); } catch { throw new ConfigError('OPEN_SANDBOX_DOMAIN must be a hostname and optional port.'); }
  if (domain.includes('://') || endpoint.username || endpoint.password || endpoint.pathname !== '/' || endpoint.search || endpoint.hash) {
    throw new ConfigError('OPEN_SANDBOX_DOMAIN must be a hostname and optional port.');
  }
  const remoteCwd = required('DSH_REMOTE_CWD');
  if (!remoteCwd.startsWith('/')) throw new ConfigError('DSH_REMOTE_CWD must be an existing writable absolute Linux directory.');
  const ttl = required('DSH_TTL_SECONDS'), timeoutSeconds = Number(ttl);
  if (!/^[1-9][0-9]*$/.test(ttl) || !Number.isSafeInteger(timeoutSeconds)) throw new ConfigError('DSH_TTL_SECONDS must be a positive safe integer.');
  const workInput = required('WORK'), evidenceInput = required('DSH_EVIDENCE_PATH'), downloadsInput = required('DSH_DOWNLOADS_DIR'), hostInput = required('DSH_HOST_OBSERVATION_DIR');
  if (![workInput, evidenceInput, downloadsInput, hostInput].every(isAbsolute)) throw new ConfigError('WORK, evidence, downloads and host observation paths must be absolute.');
  let work: string, evidencePath: string, downloadsDir: string, hostObservationDir: string;
  try {
    work = realpathSync(workInput); downloadsDir = realpathSync(downloadsInput); hostObservationDir = realpathSync(hostInput);
    assert(lstatSync(work).isDirectory() && lstatSync(downloadsDir).isDirectory() && lstatSync(hostObservationDir).isDirectory());
    accessSync(downloadsDir, constants.W_OK | constants.X_OK); accessSync(hostObservationDir, constants.R_OK | constants.X_OK);
    evidencePath = join(realpathSync(dirname(evidenceInput)), basename(evidenceInput));
  } catch { throw new ConfigError('WORK, downloads, host observation and evidence parent directories must already exist and be accessible.'); }
  if (inside(evidencePath, work) || inside(downloadsDir, work) || inside(work, downloadsDir)) {
    throw new ConfigError('Evidence and downloads must be outside WORK, in separate dedicated directories.');
  }
  if (inside(hostObservationDir, work) || inside(hostObservationDir, downloadsDir) || inside(downloadsDir, hostObservationDir)) throw new ConfigError('Host observation must be a separate directory outside WORK and downloads.');
  if (inside(evidencePath, downloadsDir)) throw new ConfigError('Evidence must be separate from downloads.');
  return {domain, protocol, apiKey: required('OPEN_SANDBOX_API_KEY'), templateId: required('DSH_TEMPLATE_ID'),
    sessionId: required('DSH_SESSION_ID'), remoteCwd, timeoutSeconds, work, evidencePath, downloadsDir, hostObservationDir};
}

type Facts = Record<string, string | number | boolean | null>;
interface Stage { name: string; status: 'passed' | 'failed' | 'skipped'; facts?: Facts; }
interface CleanupSandbox {
  sandboxId: string | null; deletionAttempted: boolean; sdkDeleteObserved: boolean;
  killPath: 'binding-kill' | 'opener-kill' | 'manager-cleanup' | null;
  kill: 'accepted' | 'unconfirmed' | 'not-attempted';
  deleted: 'confirmed-404' | 'still-present' | 'unconfirmed'; localClosed: boolean;
}
export interface E2eEvidence {
  schemaVersion: 2; mode: 'real'; success: boolean; startedAt: string; finishedAt: string;
  versions: Record<string, string>; stages: Stage[];
  cleanup: {creationOutcome: 'known' | 'unknown'; creationRequestIds: string[]; sessionsDisposed: boolean; managerClosed: boolean; sandboxes: CleanupSandbox[]};
  failures: {stage: string; code: string}[];
}
const STAGES = ['host_nonappearance_before', 'create_primary', 'readiness', 'preflight', 'binary_upload', 'agent_tools', 'download_hash',
  'close_reconnect', 'create_secondary', 'secondary_readiness', 'secondary_preflight', 'session_isolation', 'cancellation_status', 'host_nonappearance_after'];
const BYTES = Uint8Array.from([0, 255, 128, 10, 0xc3, 0x28, 0, 1]);
const hash = (bytes: Uint8Array) => createHash('sha256').update(bytes).digest('hex');
const safeId = (value: string): string | null => /^[A-Za-z0-9_.:-]{1,200}$/.test(value) ? value : null;
function tool(id: string, name: string, args: object): StreamChunk[] {
  const callId = ToolCallId(id), argumentsJson = JSON.stringify(args);
  return [{type: 'block-start', index: 0, blockType: 'tool-call'},
    {type: 'tool-call-delta', index: 0, id: callId, name, argumentsDelta: argumentsJson},
    {type: 'block-end', index: 0, block: {type: 'tool-call', id: callId, name, arguments: argumentsJson}},
    {type: 'finish', reason: {kind: 'tool-calls'}}];
}
/** Deterministic public adapter SPI. No external inference, retries or ambient credentials. */
class ScriptedAdapter extends LlmAdapter {
  readonly script: StreamChunk[][];
  constructor(note: string, marker?: string) {
    super();
    this.script = marker ? [
      tool('e2e-bash', 'bash', {description: 'Create the isolated E2E note', command: `printf '%s' '${marker}' > '${note}'`}),
      tool('e2e-read', 'read', {file_path: note}),
      tool('e2e-edit', 'edit', {file_path: note, old_string: marker, new_string: `${marker}-edited`}),
    ] : [tool('reconnected-read', 'read', {file_path: note})];
    this.script.push([{type: 'block-start', index: 0, blockType: 'text'}, {type: 'text-delta', index: 0, text: 'done'},
      {type: 'block-end', index: 0, block: {type: 'text', text: 'done'}}, {type: 'finish', reason: {kind: 'stop'}}]);
  }
  override async resolveModel(provider: string, model: string): Promise<LlmResolvedModelInfo> {return {provider, id: model, name: model};}
  async *stream(_options: GenerateOptions): AsyncIterable<StreamChunk> {
    const next = this.script.shift(); assert(next, 'E2E script exhausted'); yield* next;
  }
}
async function runTools(session: HeadlessSession, expected: string[], notePath: string, expectedRead: string): Promise<Facts> {
  let errors = 0, failures = 0; const tools: string[] = [], reads: {id: string; value: unknown}[] = [];
  const offError = session.agent.ctx.on('agent/error', () => {errors++;});
  const offResult = session.agent.ctx.on('tools/result', (exec, result) => {
    tools.push(exec.name); if (!result.isError && toolFailed(result.value)) failures++;
    if (exec.name === 'read' && !result.isError) reads.push({id: String(exec.callId), value: result.value});
  });
  try {
    const offset = session.agent.session.snapshotEvents().length;
    await session.run('Execute the fixed E2E script.');
    const outcome = summarizeRun(session.agent, offset, errors, failures);
    assert(outcome.success, 'Agent turn did not complete successfully'); assert.deepEqual(tools, expected); assert.equal(reads.length, 1);
    const read = reads[0]!;
    assert.deepEqual(read.value, {path: notePath, offset: 1, lines: [{number: 1, text: expectedRead}], totalLines: 1});
    const committed = session.agent.session.snapshotEvents().slice(offset).flatMap(event => event.type === 'tool/result' && String(event.data.message.toolCallId) === read.id ? [event.data.message] : []);
    const rendered = `<path>${notePath}</path>\n<type>file</type>\n<content>\n1: ${expectedRead}\n\n(End of file - total 1 lines)\n</content>`;
    assert.equal(committed.length, 1); assert.equal(committed[0]!.isError, false);
    assert.deepEqual(committed[0]!.content, [{type: 'text', text: rendered}]);
    return {turns: outcome.turns, toolResults: outcome.toolResults, readValueVerified: true, committedReadVerified: true, readSha256: hash(Buffer.from(expectedRead))};
  } finally {offError(); offResult();}
}
async function bounded<T>(operation: Promise<T>, ms: number): Promise<T> {
  let timer: ReturnType<typeof setTimeout> | undefined;
  try {return await Promise.race([operation, new Promise<never>((_, reject) => {timer = setTimeout(() => reject(new Error('Observation deadline')), ms);})]);}
  finally {clearTimeout(timer);}
}
/** ENOENT is absence only when the nearest existing ancestor is accessible. */
export function assertHostAbsent(paths: readonly string[]): void {
  for (const path of paths) {
    let probe = path;
    for (;;) {
      try {
        const info = lstatSync(probe);
        if (probe === path) throw new Error('Unexpected host artifact');
        assert(info.isDirectory() || info.isSymbolicLink()); accessSync(probe, constants.R_OK | constants.X_OK); break;
      } catch (error) {
        if (!(error instanceof Error) || (error as NodeJS.ErrnoException).code !== 'ENOENT') throw new Error('Host absence cannot be confirmed');
        const parent = dirname(probe); if (parent === probe) throw new Error('Host path is unverified'); probe = parent;
      }
    }
  }
}
interface CancellationObserver {sandboxId: string; command: string; id?: string; onInit(id: string): void; facts: Facts;}
async function cancellationStatus(session: HeadlessSession, binding: BoundSandbox, observer: CancellationObserver): Promise<void> {
  const shell = session.agent.ctx.root.shell, abort = new AbortController();
  let acknowledged!: () => void;
  const ack = new Promise<void>(resolve => {acknowledged = resolve;});
  observer.onInit = id => {if (!observer.id) {observer.id = id; observer.facts.commandId = safeId(id); acknowledged();}};
  try {
    const handle = await bounded(shell.execute(shell.resolve({command: observer.command, signal: abort.signal, timeoutMs: 45_000, onExpiry: 'kill'})), 5_000);
    await bounded(ack, 5_000); assert(observer.id); abort.abort(); observer.facts.abortRequested = true;
    const statusWork = (async () => {
      const deadline = Date.now() + 3_000, transport = new SdkTransport(binding.sandbox);
      try {
        do {
          const status: CommandStatus = await bounded(transport.getCommandStatus(observer.id!), Math.max(1, deadline - Date.now()));
          assert(status.id === undefined || status.id === observer.id);
          observer.facts.running = typeof status.running === 'boolean' ? status.running : null;
          observer.facts.exitCode = typeof status.exitCode === 'number' && Number.isSafeInteger(status.exitCode) ? status.exitCode : null;
          if (status.running === false) return true;
          await new Promise(resolve => setTimeout(resolve, 100));
        } while (Date.now() < deadline);
      } catch { /* Independent unknown status remains explicit. */ }
      return false;
    })();
    const resultWork = (async () => {
      const deadline = Date.now() + 5_000;
      try {
        await bounded(handle.done, 5_000);
        const result = await bounded(handle.result(), Math.max(1, deadline - Date.now()));
        observer.facts.pluginResultReturned = true; observer.facts.aborted = result.aborted; observer.facts.timedOut = result.timedOut;
        return result.aborted === true && result.timedOut === false;
      } catch (error) {
        if (error instanceof RemoteShellError) {observer.facts.aborted = error.aborted; observer.facts.timedOut = error.timedOut;}
        return false;
      }
    })();
    const [stopped, cancelled] = await Promise.all([statusWork, resultWork]);
    assert(stopped && cancelled && observer.facts.launches === 1 && observer.facts.interruptAttempted === true, 'Plugin cancellation or remote status is unconfirmed');
  } finally {abort.abort();}
}
async function verifyDeleted(manager: SandboxManager, id: string): Promise<CleanupSandbox['deleted']> {
  const deadline = Date.now() + 60_000;
  do {
    try {await bounded(manager.getSandboxInfo(id), Math.max(1, deadline - Date.now()));}
    catch (error) {return error instanceof SandboxApiException && error.statusCode === 404 ? 'confirmed-404' : 'unconfirmed';}
    if (Date.now() >= deadline) break;
    await new Promise(resolve => setTimeout(resolve, 200));
  } while (Date.now() < deadline);
  return 'still-present';
}

/** Public SDK forwarding only; local observation deadlines never retry remote operations. */
export async function runE2e(config: E2eConfig, signal?: AbortSignal): Promise<E2eEvidence> {
  const evidence: E2eEvidence = {schemaVersion: 2, mode: 'real', success: false, startedAt: new Date().toISOString(), finishedAt: '',
    versions: {integration: '0.1.0', node: process.version, opensandboxSdk: '1.1.0', dsh: '0.2.1-alpha.1', cordis: '4.0.5-alpha.1',
      opensandboxSourceBaseline: '4a7fcba5a4ded67e3856180936371673d390399b', dshSourceReference: '5badb15009ae1756c3afe0ae0cef1faafc290ccc',
      fastSandboxSourcePin: 'b702fbe71593df4f36b591e540445c251a698e7c'}, stages: [],
    cleanup: {creationOutcome: 'known', creationRequestIds: [], sessionsDisposed: true, managerClosed: true, sandboxes: []}, failures: []};
  const runId = randomUUID(), note = `dsh-e2e-${runId}.note.txt`, binary = `dsh-e2e-${runId}.binary.bin`;
  const primaryMarker = `primary-${runId}`, secondaryMarker = `secondary-${runId}`, notePath = `${config.remoteCwd}/${note}`;
  const hostPaths = [...new Set([process.cwd(), config.remoteCwd, config.hostObservationDir].flatMap(dir => [join(dir, note), join(dir, binary)]))];
  const connectionConfig = {domain: config.domain, protocol: config.protocol, apiKey: config.apiKey, disableMetrics: true, requestTimeoutSeconds: 15};
  const manager = SandboxManager.create({connectionConfig}), sessions: HeadlessSession[] = [];
  interface Owned extends CleanupSandbox {id: string; handles: Sandbox[]; binding?: BoundSandbox;}
  const owned: Owned[] = [];
  let failed = false, activeCancellation: CancellationObserver | undefined;
  type Opening = {create: string; ready: string; preflight: string; inProgress: boolean; duplicateId?: string};
  let opening: Opening | undefined;
  const record = (name: string, status: Stage['status'], facts?: Facts) => {
    if (!evidence.stages.some(stage => stage.name === name)) evidence.stages.push({name, status, ...(facts ? {facts} : {})});
  };
  const failure = (name: string, code: string, facts?: Facts) => {failed = true; record(name, 'failed', facts); evidence.failures.push({stage: name, code});};
  const stage = async <T>(name: string, operation: () => Promise<{value: T; facts: Facts}>, partialFacts?: Facts, independent = false): Promise<T | undefined> => {
    if (failed && !independent) {record(name, 'skipped'); return undefined;}
    try {if (signal?.aborted && !independent) throw new Error('Cancelled'); const result = await operation(); record(name, 'passed', result.facts); return result.value;}
    catch {failure(name, 'assertion_or_transport_failed', partialFacts); return undefined;}
  };
  const observe = (sandbox: Sandbox, ctx?: Opening): Sandbox => {
    let item = owned.find(value => value.id === sandbox.id);
    if (!item) {
      assert(ctx && sandbox.id, 'Unexpected unowned ID');
      item = {id: sandbox.id, sandboxId: safeId(sandbox.id), handles: [], deletionAttempted: false, sdkDeleteObserved: false,
        killPath: null, kill: 'not-attempted', deleted: 'unconfirmed', localClosed: true}; owned.push(item);
    }
    const owner = item; owner.handles.push(sandbox);
    const wait = sandbox.waitUntilReady.bind(sandbox), kill = sandbox.kill.bind(sandbox), close = sandbox.close.bind(sandbox);
    sandbox.waitUntilReady = async options => {await wait(options); if (ctx?.inProgress) record(ctx.ready, 'passed', {ready: true});};
    sandbox.kill = async () => {
      owner.deletionAttempted = true; owner.sdkDeleteObserved = true; owner.killPath ??= ctx?.inProgress ? 'opener-kill' : 'binding-kill'; owner.kill = 'unconfirmed';
      // Bound the actual opener-owned cleanup too. A late raw acknowledgement cannot change this observation.
      await bounded(kill(), 15_000); owner.kill = 'accepted';
    };
    let closing: Promise<void> | undefined;
    sandbox.close = () => closing ??= (async () => {
      try {await bounded(close(), 5_000);} catch (error) {owner.localClosed = false; throw error;}
    })();
    const stream = sandbox.commands.runStream.bind(sandbox.commands), interrupt = sandbox.commands.interrupt.bind(sandbox.commands);
    sandbox.commands.runStream = async function*(...args) {
      const observerAtLaunch = activeCancellation?.sandboxId === sandbox.id && Array.isArray(args[0]) && args[0].includes(activeCancellation.command) ? activeCancellation : undefined;
      if (observerAtLaunch) observerAtLaunch.facts.launches = Number(observerAtLaunch.facts.launches) + 1;
      for await (const event of stream(...args)) {
        const observer = activeCancellation === observerAtLaunch ? observerAtLaunch : undefined;
        if (observer?.sandboxId === sandbox.id && event.type === 'init' && event.text) observer.onInit(event.text);
        yield event;
      }
    };
    sandbox.commands.interrupt = async id => {
      const observer = activeCancellation?.sandboxId === sandbox.id && activeCancellation.id === id ? activeCancellation : undefined;
      if (observer) observer.facts.interruptAttempted = true;
      try {await interrupt(id); if (observer && activeCancellation === observer) observer.facts.interruptAcknowledged = true;}
      catch (error) {if (observer && activeCancellation === observer) observer.facts.interruptAcknowledged = false; throw error;}
    };
    return sandbox;
  };
  const opener = createBindingOpener({
    create: options => Sandbox.create(options),
    createFromTemplate: async options => {
      const id = options.metadata?.['dsh-binding-request']; if (typeof id === 'string') evidence.cleanup.creationRequestIds.push(id);
      const sandbox = await Sandbox.createFromTemplate(options), ctx = opening!;
      const duplicate = owned.some(item => item.id === sandbox.id);
      observe(sandbox, ctx);
      if (duplicate) {
        // Keep the new handle and the known ID, but never prepare or replace an already-owned sandbox.
        ctx.duplicateId = sandbox.id; throw new Error('Duplicate creation ID');
      }
      record(ctx.create, 'passed', {sandboxId: safeId(sandbox.id), pluginCreateTemplate: true});
      return sandbox;
    },
    connect: async options => observe(await Sandbox.connect(options)),
  });
  const openOwned = async (secondary = false): Promise<BoundSandbox | undefined> => {
    const ctx: Opening = {create: secondary ? 'create_secondary' : 'create_primary', ready: secondary ? 'secondary_readiness' : 'readiness',
      preflight: secondary ? 'secondary_preflight' : 'preflight', inProgress: true};
    if (failed) {for (const name of [ctx.create, ctx.ready, ctx.preflight]) record(name, 'skipped'); return undefined;}
    opening = ctx;
    try {
      const value = await opener({kind: 'create-template', templateId: config.templateId, timeoutSeconds: config.timeoutSeconds,
        sessionId: `${config.sessionId}-${secondary ? 'secondary' : 'primary'}`, remoteCwd: config.remoteCwd, connectionConfig,
        readyTimeoutSeconds: 60, preflightTimeoutSeconds: 10, ...(signal ? {signal} : {})});
      owned.find(item => item.id === value.descriptor.sandboxId)!.binding = value;
      record(ctx.preflight, 'passed', {linuxPythonBashWritableCwd: true});
      return value;
    } catch (error) {
      const missing = [ctx.create, ctx.ready, ctx.preflight].find(name => !evidence.stages.some(stage => stage.name === name))!;
      if (ctx.duplicateId !== undefined) {
        failure(ctx.create, 'duplicate_creation_id', {sandboxId: safeId(ctx.duplicateId), pluginCreateTemplate: true, duplicateOwnedId: true});
      } else if (error instanceof UnknownOutcomeError) {
        evidence.cleanup.creationOutcome = 'unknown'; failure(missing, 'creation_outcome_unknown', {correlationId: safeId(error.correlationId)});
      } else if (error instanceof BindingOpenError) failure(missing, 'plugin_binding_open_failed', {sandboxId: safeId(error.sandboxId), openerCleanup: error.cleanupState});
      else failure(missing, 'plugin_binding_open_failed');
      for (const name of [ctx.create, ctx.ready, ctx.preflight]) record(name, 'skipped');
      return undefined;
    } finally {ctx.inProgress = false; opening = undefined;}
  };
  const session = async (binding: BoundSandbox, marker?: string) => {
    const value = await createHeadlessSession({binding, adapter: new ScriptedAdapter(note, marker), provider: 'e2e-scripted', model: 'deterministic'});
    sessions.push(value); return value;
  };
  try {
    await stage('host_nonappearance_before', async () => {assertHostAbsent(hostPaths); return {value: true, facts: {candidatePaths: hostPaths.length, absent: true, uniqueRunId: runId}};});
    const binding = await openOwned();
    await stage('binary_upload', async () => {await new SdkTransport(binding!.sandbox).writeBytes(`${config.remoteCwd}/${binary}`, BYTES, {mode: 600});
      return {value: true, facts: {bytes: BYTES.length, sha256: hash(BYTES)}};});
    const firstSession = await stage('agent_tools', async () => {const value = await session(binding!, primaryMarker);
      return {value, facts: await runTools(value, ['bash', 'read', 'edit'], notePath, primaryMarker)};});
    await stage('download_hash', async () => {
      const sdk = new SdkTransport(binding!.sandbox), bytes = await sdk.readBytes(`${config.remoteCwd}/${binary}`);
      const chunks: Uint8Array[] = []; for await (const chunk of sdk.readBytesStream(`${config.remoteCwd}/${binary}`)) chunks.push(chunk);
      assert.equal(hash(bytes), hash(BYTES)); assert.deepEqual(Buffer.concat(chunks), Buffer.from(BYTES));
      assert.deepEqual(Buffer.from(await sdk.readBytes(notePath)), Buffer.from(`${primaryMarker}-edited`));
      const downloadFile = `${runId}.download.bin`; writeFileSync(join(config.downloadsDir, downloadFile), bytes, {flag: 'wx', mode: 0o600});
      return {value: true, facts: {bytes: bytes.length, sha256: hash(bytes), streamSha256: hash(Buffer.concat(chunks)), editedNoteVerified: true, downloadFile}};
    });
    const reconnected = await stage('close_reconnect', async () => {
      await bounded(firstSession!.dispose(), 10_000); await bounded(binding!.close(), 5_000);
      const value = await opener({kind: 'connect', sandboxId: binding!.descriptor.sandboxId, sessionId: `${config.sessionId}-primary-reconnected`,
        remoteCwd: config.remoteCwd, connectionConfig, readyTimeoutSeconds: 60, preflightTimeoutSeconds: 10, ...(signal ? {signal} : {})});
      assert.equal(value.descriptor.sandboxId, binding!.descriptor.sandboxId); owned.find(item => item.id === value.descriptor.sandboxId)!.binding = value;
      assert.deepEqual(await new SdkTransport(value.sandbox).readBytes(`${config.remoteCwd}/${binary}`), BYTES);
      return {value, facts: {sameSandboxId: true, sandboxId: safeId(value.descriptor.sandboxId), originalBytesRetained: true}};
    });
    const secondBinding = await openOwned(true);
    let liveSession: HeadlessSession | undefined;
    await stage('session_isolation', async () => {
      assert.notEqual(reconnected!.descriptor.sandboxId, secondBinding!.descriptor.sandboxId);
      const first = await session(reconnected!), second = await session(secondBinding!, secondaryMarker); liveSession = first;
      await Promise.all([runTools(first, ['read'], notePath, `${primaryMarker}-edited`), runTools(second, ['bash', 'read', 'edit'], notePath, secondaryMarker)]);
      assert.notEqual(first.agent.ctx.root, second.agent.ctx.root);
      assert.deepEqual(Buffer.from(await new SdkTransport(reconnected!.sandbox).readBytes(notePath)), Buffer.from(`${primaryMarker}-edited`));
      assert.deepEqual(Buffer.from(await new SdkTransport(secondBinding!.sandbox).readBytes(notePath)), Buffer.from(`${secondaryMarker}-edited`));
      return {value: true, facts: {distinctContexts: true, distinctSandboxIds: true, sameCwdAndFilename: true, ownCommittedReadsVerified: true, ownEditedNotesVerified: true}};
    });
    const facts: Facts = {commandId: null, abortRequested: false, pluginResultReturned: false, aborted: null, timedOut: null,
      interruptAttempted: false, interruptAcknowledged: null, running: null, launches: 0};
    await stage('cancellation_status', async () => {
      activeCancellation = {sandboxId: reconnected!.descriptor.sandboxId, command: `sleep 30 # dsh-e2e-cancel-${runId}`, onInit: () => undefined, facts};
      try {await cancellationStatus(liveSession!, reconnected!, activeCancellation); return {value: true, facts};}
      finally {activeCancellation = undefined;}
    }, facts);
  } finally {
    for (const value of sessions) try {await bounded(value.dispose(), 10_000);} catch {evidence.cleanup.sessionsDisposed = false;}
    if (!evidence.cleanup.sessionsDisposed) evidence.failures.push({stage: 'cleanup', code: 'session_disposal_failed'});
    for (const item of owned) {
      if (!item.deletionAttempted) {
        item.deletionAttempted = true; item.kill = 'unconfirmed';
        if (item.binding?.state === 'open') {
          item.killPath = 'binding-kill';
          try {await bounded(item.binding.kill(), 15_000);} catch { /* No rescue/retry DELETE. */ }
        } else {
          item.killPath = 'manager-cleanup'; item.sdkDeleteObserved = true;
          try {await bounded(manager.killSandbox(item.id), 15_000); item.kill = 'accepted';} catch { /* Unknown DELETE is not retried. */ }
        }
      }
      if (item.kill !== 'accepted' || !item.sdkDeleteObserved) evidence.failures.push({stage: 'cleanup', code: 'plugin_deletion_unconfirmed'});
      item.deleted = await verifyDeleted(manager, item.id);
      if (item.deleted !== 'confirmed-404') evidence.failures.push({stage: 'cleanup', code: 'post_kill_404_unconfirmed'});
      try {await bounded(item.binding?.close() ?? Promise.resolve(), 5_000);} catch {item.localClosed = false;}
      for (const handle of item.handles) try {await bounded(handle.close(), 5_000);} catch {item.localClosed = false;}
      if (!item.localClosed) evidence.failures.push({stage: 'cleanup', code: 'local_close_failed'});
      const {id: _, handles: __, binding: ___, ...safe} = item; evidence.cleanup.sandboxes.push(safe);
    }
    try {await bounded(manager.close(), 5_000);} catch {evidence.cleanup.managerClosed = false; evidence.failures.push({stage: 'cleanup', code: 'manager_close_failed'});}
    await stage('host_nonappearance_after', async () => {assertHostAbsent(hostPaths); return {value: true, facts: {candidatePaths: hostPaths.length, absent: true}};}, undefined, true);
  }
  assert.deepEqual(evidence.stages.map(stage => stage.name), STAGES);
  evidence.finishedAt = new Date().toISOString(); evidence.success = !evidence.failures.length && evidence.cleanup.creationOutcome === 'known';
  return evidence;
}
export function writeEvidence(config: E2eConfig, evidence: E2eEvidence): void {
  writeFileSync(config.evidencePath, `${JSON.stringify(evidence, null, 2)}\n`, {flag: 'wx', mode: 0o600});
}
export async function main(): Promise<void> {
  const config = readE2eConfig();
  // Reserve before any network activity. An existing evidence file is never overwritten.
  const fd = openSync(config.evidencePath, 'wx', 0o600), signal = new AbortController();
  const cancel = () => signal.abort(); process.on('SIGINT', cancel); process.on('SIGTERM', cancel);
  try {
    const evidence = await runE2e(config, signal.signal); writeFileSync(fd, `${JSON.stringify(evidence, null, 2)}\n`);
    console.log(JSON.stringify(evidence)); if (!evidence.success) process.exitCode = 1;
  } finally {closeSync(fd); process.off('SIGINT', cancel); process.off('SIGTERM', cancel);}
}
if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  await main().catch(error => {process.exitCode = 1; console.error(error instanceof ConfigError ? error.message :
    'Real E2E failed. Preserve evidence and inspect known IDs; no automatic retry or fallback was attempted.');});
}
