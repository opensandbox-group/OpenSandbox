// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

/**
 * Offline composition of the real Web SessionController and native AgentLoop.
 * Only persistence, the configured provider's network adapter, and ancillary
 * workspace integration are fixtures. No Agent factory is replaced. Attachment
 * admission is the production native deny-only service, not a test substitute.
 */
import assert from 'node:assert/strict';
import { mkdtemp, rm, stat } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { Context, type Plugin } from '@deepseek-ai/cordis';
import Loader from '@deepseek-ai/cordis-plugin-loader';
import AgentRegistry from '@deepseek-ai/dsh-agent';
import AgentDefaultModel from '@deepseek-ai/dsh-agent-default-model';
import AgentLoop from '@deepseek-ai/dsh-agent-loop';
import AgentPresetRegistry from '@deepseek-ai/dsh-agent-preset-registry';
import SessionController from '@deepseek-ai/dsh-api-session-controller';
import LlmRuntime, {
  LlmAdapter, ToolCallId, type GenerateOptions, type StreamChunk,
} from '@deepseek-ai/dsh-llm';
import SessionStore, {
  SessionLogOffset, type SessionEvent, type SessionHeader, type SessionId,
} from '@deepseek-ai/dsh-session';
import {
  SessionPersistence, SessionPersistenceRevision, SessionAlreadyExistsError,
  SessionAlreadyOwnedError, SessionPersistenceNotFoundError, SessionReadOnlyError,
  SessionHandleClosedError, assertContiguous, materializeAppendBatch,
  materializeCreateHeader, validateStoredEvents,
  type SessionAccess, type SessionHandle, type SessionPersistenceCreateOptions,
  type SessionPersistenceOpenOptions, type SessionPersistenceStatOptions,
  type SessionPersistenceListOptions,
} from '@deepseek-ai/dsh-session-persistence';
import SessionProjectionRegistry from '@deepseek-ai/dsh-session-projection';
import { SessionQueryEngine } from '@deepseek-ai/dsh-session-query';
import { ShellExecutor } from '@deepseek-ai/dsh-shell';
import * as ShellEnv from '@deepseek-ai/dsh-shell-env';
import SystemPrompt from '@deepseek-ai/dsh-system-prompt';
import ToolRuntime from '@deepseek-ai/dsh-tools';
import TypertRegistry from '@deepseek-ai/dsh-typert-registry';

export const fixtureModel = Object.freeze({
  provider: 'already-configured', model: 'existing-model',
});

export interface StoredSession {
  readonly header: SessionHeader;
  readonly events: SessionEvent[];
  readonly inheritedEventCount: SessionLogOffset;
}

/** Retain this object, not a running Context, to exercise a cold process restart. */
export interface ControllerStore {
  readonly sessions: Map<SessionId, StoredSession>;
  readonly writers: Set<SessionId>;
}

export function createControllerStore(): ControllerStore {
  return { sessions: new Map(), writers: new Set() };
}

export interface ModelRequestEvidence {
  readonly provider: string;
  readonly model: string;
  readonly sessionId: string | undefined;
  readonly messages: GenerateOptions['messages'];
  readonly tools: GenerateOptions['tools'];
}

export type OfflineResponse =
  | { readonly text: string }
  | { readonly tool: string; readonly arguments: Record<string, unknown> };

/** Scripted adapter registered at the existing configured provider boundary. */
export class OfflineAdapter extends LlmAdapter {
  private readonly scripts = new Map<string, OfflineResponse[]>();
  private callCount = 0;
  readonly requests: ModelRequestEvidence[] = [];

  script(sessionId: string, ...responses: OfflineResponse[]): void {
    this.scripts.set(sessionId, [...responses]);
  }

  override async listModels(provider: string) {
    return [{ provider, id: fixtureModel.model, name: fixtureModel.model }];
  }

  override async resolveModel(provider: string, model: string) {
    return { provider, id: model, name: model };
  }

  override async *stream(options: GenerateOptions): AsyncIterable<StreamChunk> {
    options.signal?.throwIfAborted();
    this.requests.push({
      provider: options.provider,
      model: options.model,
      sessionId: options.sessionId === undefined ? undefined : String(options.sessionId),
      messages: structuredClone(options.messages),
      tools: structuredClone(options.tools),
    });
    const response = this.scripts.get(String(options.sessionId))?.shift()
      ?? { text: 'offline configured adapter completed' };
    if ('tool' in response) {
      const id = ToolCallId(`offline-call-${++this.callCount}`);
      const argumentsText = JSON.stringify(response.arguments);
      yield { type: 'block-start', index: 0, blockType: 'tool-call' };
      yield { type: 'tool-call-delta', index: 0, id, name: response.tool, argumentsDelta: argumentsText };
      yield { type: 'block-end', index: 0, block: { type: 'tool-call', id, name: response.tool, arguments: argumentsText } };
      yield { type: 'finish', reason: { kind: 'tool-calls' } };
      return;
    }
    yield { type: 'block-start', index: 0, blockType: 'text' };
    yield { type: 'text-delta', index: 0, text: response.text };
    yield { type: 'block-end', index: 0, block: { type: 'text', text: response.text } };
    yield { type: 'finish', reason: { kind: 'stop' } };
  }
}

class MemoryPersistence extends SessionPersistence {
  private readonly store: ControllerStore;
  private readonly closes: string[];
  private readonly ownedWriters = new Set<SessionId>();
  private readonly handles = new Set<SessionHandle>();

  constructor(ctx: Context, options: { store: ControllerStore; closes: string[] }) {
    super(ctx);
    this.store = options.store;
    this.closes = options.closes;
    // The persistence SPI owns routing of published events, not AgentLoop.
    // Memory commits synchronously, so session/flush has no buffered work.
    ctx.on('session/event', (session, event) => {
      if (!this.ownedWriters.has(session.id)) return;
      const row = this.store.sessions.get(session.id)!;
      const batch = materializeAppendBatch([event]);
      assertContiguous(session.id, batch, row.events.length);
      row.events.push(...batch);
    });
    ctx.on('session/flush', () => this.flush());
    ctx.effect(() => async () => {
      await Promise.all([...this.handles].map(handle => handle.close()));
    });
  }

  async create(header: SessionHeader, options: SessionPersistenceCreateOptions = {}): Promise<SessionHandle> {
    options.signal?.throwIfAborted();
    if (this.store.sessions.has(header.id)) throw new SessionAlreadyExistsError(header.id);
    const inheritedEventCount = options.inheritedEventCount ?? SessionLogOffset(0);
    if (header.isSeeded !== true) assert.equal(inheritedEventCount, 0);
    this.store.sessions.set(header.id, {
      header: materializeCreateHeader(header), events: [], inheritedEventCount,
    });
    return this.open(header.id, 'write');
  }

  async open(id: SessionId, access: SessionAccess, options: SessionPersistenceOpenOptions = {}): Promise<SessionHandle> {
    options.signal?.throwIfAborted();
    const row = this.store.sessions.get(id);
    if (row === undefined) throw new SessionPersistenceNotFoundError(id);
    if (access === 'write') {
      if (this.store.writers.has(id)) throw new SessionAlreadyOwnedError(id);
      this.store.writers.add(id);
      this.ownedWriters.add(id);
    }
    let closed = false;
    const check = (operation: string, write = false) => {
      if (closed) throw new SessionHandleClosedError(id, operation);
      if (write && access !== 'write') throw new SessionReadOnlyError(id, operation);
    };
    const store = this.store;
    const closes = this.closes;
    const close = async () => {
      if (closed) return;
      closed = true;
      if (access === 'write') {
        store.writers.delete(id);
        this.ownedWriters.delete(id);
      }
      this.handles.delete(handle);
      closes.push(String(id));
    };
    const handle: SessionHandle = {
      id, access, header: structuredClone(row.header), inheritedEventCount: row.inheritedEventCount,
      async read(offset = 0, length, readOptions = {}) {
        check('read'); readOptions.signal?.throwIfAborted();
        const validated = validateStoredEvents(row.header, structuredClone(row.events));
        return { events: validated.slice(offset, length === undefined ? undefined : offset + length), eventState: 'detached' };
      },
      async append(events, appendOptions = {}) {
        check('append', true); appendOptions.signal?.throwIfAborted();
        const batch = materializeAppendBatch(events);
        assertContiguous(id, batch, row.events.length);
        row.events.push(...batch);
      },
      async flush(flushOptions = {}) {
        check('flush', true); flushOptions.signal?.throwIfAborted();
      },
      close,
      [Symbol.asyncDispose]: close,
    };
    this.handles.add(handle);
    return handle;
  }

  async flush(): Promise<void> {}

  async stat(id: SessionId, options: SessionPersistenceStatOptions = {}) {
    options.signal?.throwIfAborted();
    const row = this.store.sessions.get(id);
    return row === undefined ? undefined : {
      header: structuredClone(row.header),
      revision: SessionPersistenceRevision(`memory:${row.events.length}`),
      eventCount: row.events.length,
    };
  }

  async list(options: SessionPersistenceListOptions = {}) {
    options.signal?.throwIfAborted();
    return Promise.all([...this.store.sessions.keys()].map(async id => (await this.stat(id))!));
  }
}

/** All create/resume observation and projection behavior is the real SPI. */
class OfflineSessionQuery extends SessionQueryEngine {
  async searchSessions(): Promise<never> { throw new Error('Offline fixture does not implement search'); }
  async searchEvents(): Promise<never> { throw new Error('Offline fixture does not implement search'); }
}

class HostShellTrap extends ShellExecutor {
  private readonly calls: string[];
  constructor(ctx: Context, calls: string[]) { super(ctx); this.calls = calls; }
  resolve(): never { this.calls.push('shell.resolve'); throw new Error('Host shell must not run'); }
  async execute(): Promise<never> { this.calls.push('shell.execute'); throw new Error('Host shell must not run'); }
}

export interface ControllerFixtureOptions {
  /** Reusing the store after dispose preserves only native Session persistence. */
  readonly store?: ControllerStore;
}

/**
 * Install the actual exported native plugin before AgentLoop and SessionController.
 * Native owns deny-only root fs/fileUploads; this fixture never shadows those gates.
 */
export async function createControllerFixture<Config>(
  plugin: Plugin<Config>, config: Config, options: ControllerFixtureOptions = {},
) {
  const root = new Context();
  const store = options.store ?? createControllerStore();
  const cwd = await mkdtemp(join(tmpdir(), 'dsh-native-controller-'));
  const adapter = new OfflineAdapter();
  const hostCalls: string[] = [];
  const persistenceCloses: string[] = [];
  const bootstrapPaths: string[] = [];
  const lifecycle: string[] = [];
  let disposed = false;
  const dispose = async () => {
    if (disposed) return;
    disposed = true;
    try { await root.fiber.dispose(); }
    finally { await rm(cwd, { recursive: true, force: true }); }
  };
  try {
    await root.plugin(LlmRuntime);
    await root.plugin(SessionStore);
    await root.plugin(SessionProjectionRegistry);
    await root.plugin(SystemPrompt);
    await root.plugin(ToolRuntime);
    await root.plugin(AgentRegistry);
    await root.plugin(MemoryPersistence, { store, closes: persistenceCloses });
    await root.plugin(OfflineSessionQuery);
    await root.plugin(TypertRegistry);
    await root.plugin(Loader, { baseUrl: import.meta.url });
    await root.plugin(AgentPresetRegistry, { default: 'opensandbox' });
    await root.plugin(AgentDefaultModel, fixtureModel);
    await root.plugin(ShellEnv, { dshHome: cwd });
    await root.plugin(HostShellTrap, hostCalls);
    root.llm.registerAdapter([fixtureModel.provider], adapter);
    const originalDefaultModel = root.agentDefaultModel;
    const originalModelSelection = root.agentDefaultModel.currentSelection();

    root.provide('workspaceRegistry', {
      archivedSessionIds: [], get: () => undefined, list: () => [],
    });
    root.on('agent/created', ({ agent, source }) => {
      lifecycle.push(`agent-created:${agent.id}:${source}`);
      // The real controller calls node:fs mkdir(cwd) before fresh composition.
      // Resume does not mkdir, so it must not be counted as host bootstrap.
      if (source === 'startup' && agent.session.header.cwd !== undefined) {
        bootstrapPaths.push(agent.session.header.cwd);
      }
      return undefined;
    });
    root.on('agent/disposed', ({ agent }) => { lifecycle.push(`agent-disposed:${agent.id}`); });

    const nativeFiber = await root.plugin(plugin, config);
    assert.ok(root.get('fs'), 'Native plugin must retain deny-only root fs for controller composition');
    assert.ok(root.get('fileUploads'), 'Native plugin must retain disabled fileUploads for text prompts');
    await root.plugin(AgentLoop, { agents: [] });
    const controllerPlugin: Plugin = {
      name: 'offline-real-session-controller',
      inject: SessionController.inject,
      apply(ctx) {
        const forbidden = async (): Promise<never> => {
          hostCalls.push('native.open'); throw new Error('Host native opener must not run');
        };
        new SessionController(ctx, { nativeOpen: false }, {
          openPath: forbidden, revealPath: forbidden,
          openFileApplication: forbidden, fileApplications: forbidden, canOpenPath: () => false,
        });
      },
    };
    let controllerFiber = await root.plugin(controllerPlugin);
    assert.ok(root.get('sessionController'), 'Real SessionController must be active');

    return {
      root, cwd, nativeFiber, adapter,
      modelRequests: adapter.requests,
      store, stored: store.sessions,
      hostCalls, persistenceCloses, bootstrapPaths, lifecycle,
      originalDefaultModel, originalModelSelection,
      /** Confirm the native bootstrap directory is a test-owned host location. */
      async verifyBootstrap() {
        for (const path of bootstrapPaths) {
          assert.ok(path === cwd || path.startsWith(`${cwd}/`), 'Unexpected host bootstrap path');
          assert.ok((await stat(path)).isDirectory());
        }
      },
      /** Unload only the real API owner, draining its Agents; retain native owner. */
      async restartController() {
        await controllerFiber.dispose();
        controllerFiber = await root.plugin(controllerPlugin);
        assert.ok(root.get('sessionController'));
      },
      dispose,
    };
  } catch (error) {
    await dispose();
    throw error;
  }
}
