// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

import type { Context, Fiber } from '@deepseek-ai/cordis';
import z from '@deepseek-ai/schemastery';
import type { Agent } from '@deepseek-ai/dsh-agent';
import type { ToolDefinition } from '@deepseek-ai/dsh-tools';
import type {} from '@deepseek-ai/dsh-agent-preset-registry';
import type {} from '@deepseek-ai/dsh-system-prompt';
import { validateNativeConfig } from './config.js';
import { NativeOwner } from './owner.js';
import type { NativeConfig } from './config.js';
import type { NativeOwnerDependencies, NativeRoute } from './owner.js';
import { NativeShell, NativeFileSystem, DeniedHostFileSystem, DeniedAttachments, installDeniedFileUploads } from './services.js';
import { installNativeTools } from './tools.js';

export type { NativeConfig } from './config.js';
export { validateNativeConfig } from './config.js';
export { NativeOwner } from './owner.js';
export type { NativeRoute, NativeStatus, NativeOwnerDependencies } from './owner.js';
export const name = 'opensandbox-native';
export const inject = ['agents', 'tools', 'shellEnv', 'systemPrompt', 'agentPresets'];
export const Config = z.object({
  domain: z.string().required(),
  protocol: z.union(['http', 'https']).default('https'),
  apiKey: z.string().role('secret'),
  kind: z.union(['create-template', 'create-image']).required(),
  templateId: z.string(), image: z.string(),
  remoteCwd: z.string().default('/tmp'),
  ttlSeconds: z.natural().min(1).default(600),
  requestTimeoutSeconds: z.natural().min(1).default(15),
  readyTimeoutSeconds: z.natural().min(1).default(30),
  preflightTimeoutSeconds: z.natural().min(1).default(10),
  settlementTimeoutMs: z.natural().min(1).default(3000),
});

/** The dependency boundary is for offline SDK/provider tests, not plugin configuration. */
export function createNativePlugin(dependencies?: NativeOwnerDependencies) {
  return { name, inject, Config, apply: (ctx: Context, config: NativeConfig) => mount(ctx, config, dependencies) };
}

async function mount(ctx: Context, input: NativeConfig, dependencies?: NativeOwnerDependencies): Promise<void> {
  const config = validateNativeConfig(input);
  let active = true;
  const owner = new NativeOwner(ctx.root, config, agent => ctx.agents.get(agent.id) === agent, dependencies);
  const installs = new Map<Agent, Promise<void>>();
  const pendingInstalls = new Set<Promise<void>>();
  const routes = new Map<Agent, NativeRoute>();
  const definitions = new Map<Agent, ReadonlyMap<string, ToolDefinition>>();
  const worlds = new Map<Agent, Fiber>();
  const denied = 'OpenSandbox requires a live, fully composed native session and an owned tool.';

  const check = (agent: Agent | undefined, tool: string): string | undefined => {
    if (!active || !agent || ctx.agents.get(agent.id) !== agent) return denied;
    const owned = definitions.get(agent)?.get(tool);
    if (!owned || ctx.tools.get(tool, agent) !== owned) return denied;
    try {
      const route = routes.get(agent);
      if (!route) return denied;
      if (tool.startsWith('opensandbox_')) route.status();
      else route.assertActive();
    } catch { return denied; }
  };
  ctx.tools.guard(exec => check(exec.agent, exec.name));
  ctx.effect(() => async () => {
    active = false;
    // Admission closes synchronously, before any wait or provider teardown.
    const close = Promise.allSettled([owner.dispose()]);
    await Promise.allSettled(pendingInstalls);
    const settled = await Promise.allSettled([...worlds.values()].map(world => world.dispose()));
    const [ownerResult] = await close;
    if (ownerResult?.status === 'rejected') throw ownerResult.reason;
    if (settled.some(result => result.status === 'rejected')) throw new Error('Native session composition did not drain cleanly.');
  }, 'opensandbox native owner drain');

  const install = (agent: Agent, source: 'startup' | 'resume'): Promise<void> => {
    const existing = installs.get(agent);
    if (existing) return existing;
    const work = (async () => {
      if (!active || ctx.agentPresets.composedPreset(agent.ctx) !== 'opensandbox') throw new Error('The OpenSandbox profile requires its explicit native preset.');
      const route = owner.attach(agent);
      routes.set(agent, route);
      agent.ctx.effect(() => async () => {
        try { await route.dispose(); }
        finally {
          routes.delete(agent); definitions.delete(agent); installs.delete(agent); worlds.delete(agent);
        }
      }, 'release native Agent routing metadata after drain');
      // Registrations outside the removable tool world survive plugin unload, until Agent teardown.
      agent.ctx.tools.restrict({ allow: [] });
      agent.ctx.tools.presentAs('native');
      agent.ctx.tools.guard(exec => check(exec.agent, exec.name));
      const world = agent.ctx.isolate('shell').isolate('fs');
      const fiber = await world.plugin({
        name: 'opensandbox-session-tools',
        async apply(local: Context) {
          local.effect(() => () => route.dispose(), 'await native Agent provider drain');
          await local.plugin(NativeShell, { route });
          await local.plugin(NativeFileSystem, { route });
          local.systemPrompt.variable('cwd', () => route.remoteCwd);
          local.systemPrompt.section({
            name: 'opensandbox-remote-paths', order: local.systemPrompt.getSectionOrder('TOOL_BASH'),
            text: `Commands and files run in the remote OpenSandbox working directory ${route.remoteCwd}. Host workspace paths are not mapped. Check command exit codes. Host previews, uploads, jobs, terminals, delegation and search are not supported.`,
          });
          definitions.set(agent, await installNativeTools(local, agent, route));
        },
      });
      worlds.set(agent, fiber);
      if (!active) throw new Error('OpenSandbox native composition was unloaded.');
      owner.observeCreated(agent, source);
    })();
    installs.set(agent, work);
    pendingInstalls.add(work);
    void work.then(() => pendingInstalls.delete(work), () => pendingInstalls.delete(work));
    return work;
  };

  // The alpha public preset SPI binds a shared revision, not a per-Agent constructor.
  // The real controller mounts that preset in unpublished setup; this serial announcement
  // is rollback-covered and awaited before the native loop can release queued model work.
  ctx.on('agent/created', async ({ agent, source }) => { await install(agent, source === 'startup' ? 'startup' : 'resume'); return undefined; });
  await ctx.plugin(DeniedHostFileSystem);
  await ctx.plugin(DeniedAttachments);
  installDeniedFileUploads(ctx);
  const unregisterPreset = await ctx.agentPresets.register({
    id: 'opensandbox', name: 'OpenSandbox',
    description: 'Native Agent and configured model, with session-owned remote shell and file tools.', plugins: [],
  });
  ctx.effect(() => unregisterPreset, 'opensandbox preset registration');
  // Existing Agents are never freshly admitted, even if their durable log is empty.
  await Promise.all(ctx.agents.list().map(agent => install(agent, 'resume')));
  if (!active) throw new Error('OpenSandbox native composition was unloaded.');
  ctx.provide('opensandboxNativeReady', Object.freeze({
    preset: 'opensandbox' as const,
    assertReady() { if (!active) throw new Error('OpenSandbox native composition is unavailable.'); },
  }));
}

export const apply = (ctx: Context, config: NativeConfig): Promise<void> => mount(ctx, config);
export default { name, inject, Config, apply };
