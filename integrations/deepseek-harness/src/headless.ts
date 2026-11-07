// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

import { Context, Inject } from '@deepseek-ai/cordis';
import type { Plugin } from '@deepseek-ai/cordis';
import LlmRuntime, { createUserMessage, HarnessError, LlmAdapter, LlmError } from '@deepseek-ai/dsh-llm';
import type { GenerateOptions, LlmErrorOptions, PreparedAdapterCall, StreamChunk } from '@deepseek-ai/dsh-llm';
import SessionStore, { SessionId } from '@deepseek-ai/dsh-session';
import SessionProjectionRegistry from '@deepseek-ai/dsh-session-projection';
import SystemPrompt from '@deepseek-ai/dsh-system-prompt';
import ToolRuntime from '@deepseek-ai/dsh-tools';
import AgentRegistry from '@deepseek-ai/dsh-agent';
import type { Agent, AgentHandle } from '@deepseek-ai/dsh-agent';
import AgentLoop from '@deepseek-ai/dsh-agent-loop';
import * as ShellEnv from '@deepseek-ai/dsh-shell-env';
import * as ToolBash from '@deepseek-ai/dsh-tool-bash';
import * as ToolFs from '@deepseek-ai/dsh-tool-fs';
import * as FsObservationPolicy from '@deepseek-ai/dsh-fs-observation-policy';
import { BindingClosedError } from './errors.js';
import { RemoteShell } from './shell.js';
import { RemoteFileSystem } from './filesystem.js';
import type { BindingDescriptor, BoundSandbox } from './types.js';

type AdapterStage = 'providerInfo' | 'providerRetryPolicy' | 'imageRequestPricing' | 'priceImages' |
  'listModels' | 'resolveModel' | 'prepareCall' | 'stream' | 'preparedStream';
const SAFE_LLM_CODES = new Set([
  'UNKNOWN', 'ABORTED', 'AUTH', 'MISSING_CREDENTIAL', 'INVALID_CREDENTIAL', 'RATE_LIMIT', 'SERVER',
  'TIMEOUT', 'TRANSPORT', 'EMPTY_RESPONSE', 'CONTEXT_WINDOW_EXCEEDED', 'QUOTA', 'ACCOUNT_QUOTA',
  'IMAGE_OFFLOAD_REQUIRED', 'NO_ADAPTER', 'INVALID_CONFIG', 'UNKNOWN_MODEL', 'UNSUPPORTED_OPTION',
  'UNSUPPORTED_CONTENT', 'UNSUPPORTED_REASONING_EFFORT',
]);

/** Read only data properties. An SDK's diagnostic getter/proxy must never run during sanitization. */
function dataValue(value: unknown, key: string): unknown {
  if ((typeof value !== 'object' || value === null) && typeof value !== 'function') return undefined;
  try {
    const descriptor = Object.getOwnPropertyDescriptor(value, key);
    return descriptor && 'value' in descriptor ? descriptor.value : undefined;
  } catch { return undefined; }
}

/** Preserve the adapter SPI while keeping its failure objects outside dsh's diagnostic surfaces. */
class SafeLlmAdapter extends LlmAdapter {
  readonly #adapter: LlmAdapter;
  readonly #retryCodes = new Map<string, string>();
  constructor(adapter: LlmAdapter) { super(); this.#adapter = adapter; }
  #code(code: unknown): string {
    if (typeof code !== 'string') return 'UNKNOWN';
    if (SAFE_LLM_CODES.has(code)) return code;
    return this.#retryCodes.get(code) ?? 'ADAPTER_FAILURE';
  }
  #failure(stage: AdapterStage, value: unknown, inBand = false): LlmError {
    let typed = false, regularError = false;
    try { typed = value instanceof HarnessError; regularError = value instanceof Error; } catch { /* Hostile thrown proxies are unknown failures. */ }
    const facts = inBand ? value : dataValue(value, 'failure');
    const carriedCode = dataValue(facts, 'code'), message = dataValue(facts, 'message'), requestId = dataValue(facts, 'requestId');
    const status = dataValue(facts, 'status'), delay = dataValue(facts, 'providerRetryAfterMs'), images = dataValue(facts, 'offloadImages');
    // The published runtime also recognizes validated neutral failure records carried by ordinary
    // Error objects. Preserve that routing behavior without carrying their diagnostic strings.
    const validFacts = typeof carriedCode === 'string' && carriedCode.length > 0 && typeof message === 'string' && message.length > 0 &&
      (status === undefined || typeof status === 'number' && Number.isInteger(status) && status >= 100 && status <= 599) &&
      (delay === undefined || typeof delay === 'number' && Number.isFinite(delay) && delay > 0) &&
      (images === undefined || typeof images === 'number' && Number.isSafeInteger(images) && images > 0) &&
      (requestId === undefined || typeof requestId === 'string' && requestId.length > 0);
    const ownCode = dataValue(value, 'code');
    let classification: unknown;
    if (inBand) classification = validFacts ? carriedCode : undefined;
    else if (typed || regularError && validFacts && ownCode === carriedCode) classification = ownCode;
    const code = this.#code(classification);
    const options: LlmErrorOptions = {
      ...(typeof status === 'number' && Number.isInteger(status) && status >= 100 && status <= 599 ? { status } : {}),
      ...(typeof delay === 'number' && Number.isFinite(delay) && delay > 0 ? { providerRetryAfterMs: delay } : {}),
      ...(code === 'IMAGE_OFFLOAD_REQUIRED' && typeof images === 'number' && Number.isSafeInteger(images) && images > 0 ? { offloadImages: images } : {}),
    };
    // Only static text/classification and checked numeric facts survive. Never retain the original
    // exception, message, stack, request id, cause, headers, body or arbitrary diagnostic fields.
    return Object.assign(new LlmError(`LLM adapter ${stage} failed.`, code, options), { stage });
  }
  #sync<T>(stage: AdapterStage, operation: () => T): T {
    try { return operation(); } catch (error) { throw this.#failure(stage, error); }
  }
  async #async<T>(stage: AdapterStage, operation: () => T | PromiseLike<T>): Promise<T> {
    try { return await operation(); } catch (error) { throw this.#failure(stage, error); }
  }
  override providerInfo(...args: Parameters<LlmAdapter['providerInfo']>): ReturnType<LlmAdapter['providerInfo']> {
    return this.#sync('providerInfo', () => structuredClone(this.#adapter.providerInfo(...args)));
  }
  override providerRetryPolicy(...args: Parameters<LlmAdapter['providerRetryPolicy']>): ReturnType<LlmAdapter['providerRetryPolicy']> {
    return this.#sync('providerRetryPolicy', () => {
      const policy = structuredClone(this.#adapter.providerRetryPolicy(...args));
      if (policy?.mode !== 'normal') return policy;
      // Custom provider codes are opaque strings, potentially diagnostic data. Map only configured
      // codes to safe session-local identities and apply the same mapping to failures, preserving
      // retry eligibility without publishing those strings or making unrelated unknown errors retryable.
      return { ...policy, retryableCodes: policy.retryableCodes.map(code => {
        if (SAFE_LLM_CODES.has(code)) return code;
        let safe = this.#retryCodes.get(code);
        if (!safe) { safe = `ADAPTER_RETRY_${this.#retryCodes.size + 1}`; this.#retryCodes.set(code, safe); }
        return safe;
      }) };
    });
  }
  override imageRequestPricing(...args: Parameters<LlmAdapter['imageRequestPricing']>): ReturnType<LlmAdapter['imageRequestPricing']> {
    return this.#sync('imageRequestPricing', () => {
      const pricing = this.#adapter.imageRequestPricing(...args);
      if (!pricing) return pricing;
      const priceImages = pricing.priceImages;
      return { priceImages: images => this.#sync('priceImages', () => structuredClone(priceImages.call(pricing, images))) };
    });
  }
  override listModels(...args: Parameters<LlmAdapter['listModels']>): ReturnType<LlmAdapter['listModels']> {
    return this.#async('listModels', async () => structuredClone(await this.#adapter.listModels(...args)));
  }
  override resolveModel(...args: Parameters<LlmAdapter['resolveModel']>): ReturnType<LlmAdapter['resolveModel']> {
    return this.#async('resolveModel', async () => structuredClone(await this.#adapter.resolveModel(...args)));
  }
  override prepareCall(...args: Parameters<LlmAdapter['prepareCall']>): ReturnType<LlmAdapter['prepareCall']> {
    return this.#async('prepareCall', async (): Promise<PreparedAdapterCall> => {
      const prepared = await this.#adapter.prepareCall(...args);
      const model = structuredClone(prepared.model), dispatch = prepared.stream;
      return { model, stream: options => this.#stream(this.#sync('preparedStream', () => dispatch.call(prepared, options)), 'preparedStream') };
    });
  }
  override stream(options: GenerateOptions): AsyncIterable<StreamChunk> {
    return this.#stream(this.#sync('stream', () => this.#adapter.stream(options)), 'stream');
  }
  async *#stream(source: AsyncIterable<StreamChunk>, stage: AdapterStage): AsyncIterable<StreamChunk> {
    try {
      for await (const produced of source) {
        // Materialize data before it leaves this guard: consumers must never trigger a lazy
        // adapter getter/proxy later. Callable SPI items are delegated separately, not cloned.
        const chunk = structuredClone(produced);
        if (chunk.type === 'finish' && (chunk.reason.kind === 'error' || chunk.reason.kind === 'aborted')) {
          yield { type: 'finish', reason: { kind: chunk.reason.kind, failure: this.#failure(stage, chunk.reason.failure, true).failure } };
        } else yield chunk;
      }
    } catch (error) { throw this.#failure(stage, error); }
  }
}

/** All routing and model configuration is explicit; no default model or host provider is loaded. */
export interface HeadlessSessionOptions {
  /** An already-open, application-preflighted sandbox binding. Ownership remains with the caller. */
  binding: BoundSandbox;
  /** Caller-owned adapter, registered only in this session's private Context. */
  adapter: LlmAdapter;
  provider: string;
  model: string;
}

/** An ephemeral dsh Agent with remote-only shell/filesystem tools and guarded editing. */
export interface HeadlessSession {
  readonly descriptor: BindingDescriptor;
  readonly agent: Agent;
  /** Submit a user message through the public Agent API and await whole-agent quiescence. */
  run(text: string): Promise<void>;
  /** Drain the Agent, stop owned commands, and dispose providers/Context. Never close or kill the binding. */
  dispose(): Promise<void>;
}

function requireServices(ctx: Context, services: readonly string[]): void {
  for (const service of services) {
    if (ctx.get(service) === undefined) throw new Error(`Headless composition: required service "${service}" is missing.`);
  }
}

/**
 * Compose one private root Context per live binding. The public kernel allowlist deliberately
 * excludes stock headless/base launchers, host subprocess/filesystem search, PTC and jobs.
 * The binding's caller explicitly chooses close/kill after disposing this session.
 */
export async function createHeadlessSession(options: HeadlessSessionOptions): Promise<HeadlessSession> {
  const { binding, adapter, provider, model } = options;
  if (binding.state !== 'open') throw new BindingClosedError(binding.descriptor.sandboxId);
  if (typeof provider !== 'string' || !provider.trim() || typeof model !== 'string' || !model.trim()) {
    throw new Error('Headless composition requires explicit non-empty provider and model names.');
  }
  const descriptor = Object.freeze({ ...binding.descriptor });
  const ctx = new Context();
  let handle: AgentHandle | undefined, shell: RemoteShell | undefined, fs: RemoteFileSystem | undefined;
  let disposed = false, disposing: Promise<void> | undefined;
  const dispose = (): Promise<void> => {
    if (disposing) return disposing;
    disposed = true;
    disposing = (async () => {
      const failures: unknown[] = [];
      // Keep services available while the Agent's active tool calls unwind. Explicit provider
      // joins precede Context teardown, whose effect disposers are idempotent second passes.
      for (const cleanup of [() => handle?.dispose(), () => shell?.dispose(), () => fs?.dispose(), () => ctx.fiber.dispose()]) {
        try { await cleanup(); } catch (error) { failures.push(error); }
      }
      if (failures.length) throw new AggregateError(failures, 'Headless session disposal failed.');
    })();
    return disposing;
  };
  const mount = async (plugin: Plugin, config?: unknown, service?: string): Promise<void> => {
    requireServices(ctx, Object.keys(Inject.resolve(plugin.inject)));
    await ctx.plugin(plugin, config);
    if (service) requireServices(ctx, [service]);
  };
  try {
    await mount(LlmRuntime, undefined, 'llm');
    await mount(SessionStore, undefined, 'sessions');
    await mount(SessionProjectionRegistry, undefined, 'sessionProjections');
    await mount(SystemPrompt, undefined, 'systemPrompt');
    await mount(ToolRuntime, undefined, 'tools');
    await mount(AgentRegistry, undefined, 'agents');
    await mount(AgentLoop, { agents: [] }, 'agentLoop');
    ctx.llm.registerAdapter([provider], new SafeLlmAdapter(adapter));
    await mount(RemoteShell, { binding }, 'shell'); shell = ctx.shell as RemoteShell;
    await mount(RemoteFileSystem, { binding }, 'fs'); fs = ctx.fs as RemoteFileSystem;
    await fs.ready();
    await mount(ShellEnv, { dshHome: descriptor.remoteCwd }, 'shellEnv');
    await mount(ToolBash, { enableRunInBackground: false, promoteOnTimeout: false });
    await mount(ToolFs, {});
    await mount(FsObservationPolicy);
    requireServices(ctx, ['llm', 'sessions', 'sessionProjections', 'systemPrompt', 'tools', 'agents', 'agentLoop', 'shell', 'fs', 'shellEnv']);
    handle = await ctx.agents.create({ sessionId: SessionId(descriptor.sessionId), meta: { cwd: descriptor.remoteCwd }, agentOptions: { provider, model } });
    const agent = handle.agent;
    return Object.freeze({
      descriptor, agent,
      run: async (text: string): Promise<void> => {
        if (disposed) throw new Error('Headless session is disposed.');
        if (typeof text !== 'string') throw new TypeError('Headless session input must be text.');
        agent.followup(createUserMessage({ content: [{ type: 'text', text }], source: { kind: 'user' } }));
        await agent.whenIdle();
      },
      dispose,
    });
  } catch (error) {
    try { await dispose(); } catch (cleanupError) { throw new AggregateError([error, cleanupError], 'Headless composition failed and cleanup was incomplete.'); }
    throw error;
  }
}
