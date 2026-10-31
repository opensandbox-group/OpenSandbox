// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

import type { Context } from '@deepseek-ai/cordis';
import type { Agent } from '@deepseek-ai/dsh-agent';
import { createScope } from '@deepseek-ai/dsh-scope';
import { ToolArgsError, validateJsonSchemaValue } from '@deepseek-ai/dsh-tools';
import type { ToolDefinition, ToolRunContext } from '@deepseek-ai/dsh-tools';
import * as ToolBash from '@deepseek-ai/dsh-tool-bash';
import * as ToolFs from '@deepseek-ai/dsh-tool-fs';
import * as FsObservationPolicy from '@deepseek-ai/dsh-fs-observation-policy';
import type { NativeRoute } from './owner.js';

export const NATIVE_TOOL_NAMES = Object.freeze([
  'bash', 'read', 'write', 'edit', 'opensandbox_status', 'opensandbox_reconnect', 'opensandbox_delete',
] as const);

/** Preserve symlink-sensitive dot segments for resolution in the remote execution world. */
export function remotePath(path: string, remoteCwd: string): string {
  if (typeof path !== 'string' || !path.trim() || /[\x00-\x1f\x7f]/.test(path)) {
    throw new ToolArgsError(['The remote path must be non-empty and contain no control characters.']);
  }
  return path.startsWith('/') ? path : `${remoteCwd}/${path}`;
}
function validate(definition: ToolDefinition, args: unknown): Record<string, unknown> {
  const errors = validateJsonSchemaValue(definition.parameters, args, '');
  const properties = definition.parameters.properties ?? {};
  if (args && typeof args === 'object' && !Array.isArray(args) && Object.keys(args).some(key => !Object.hasOwn(properties, key))) {
    errors.push('Unsupported argument for the native OpenSandbox tool.');
  }
  if (errors.length) throw new ToolArgsError(errors);
  return args as Record<string, unknown>;
}
function actor(agent: Agent, route: NativeRoute, exec: ToolRunContext, execution = true): void {
  if (exec.agent !== agent) throw new Error('OpenSandbox requires the exact owning Agent.');
  if (execution) route.assertActive();
  else route.status();
}

/** Keep the original tool fibers alive privately; only the seven owned wrappers are public. */
export async function installNativeTools(ctx: Context, agent: Agent, route: NativeRoute): Promise<ReadonlyMap<string, ToolDefinition>> {
  const privateKey = {};
  const originals = createScope(ctx, privateKey);
  await originals.ctx.plugin(FsObservationPolicy);
  await originals.ctx.plugin(ToolBash, { enableRunInBackground: false, promoteOnTimeout: false });
  await originals.ctx.plugin(ToolFs, {});

  for (const name of ['bash', 'read', 'write', 'edit'] as const) {
    const original = ctx.tools.get(name, privateKey);
    if (!original) throw new Error('Required native stock tool is unavailable.');
    ctx.tools.register({
      ...original,
      description: `${original.description} Relative paths use the configured remote working directory, not the host workspace.`,
      async execute(input, exec) {
        actor(agent, route, exec);
        const args = validate(original, input);
        if (name === 'bash' && (!(args.command as string).trim() || (args.command as string).includes('\0') || !(args.description as string).trim())) {
          throw new ToolArgsError(['A non-empty command without NUL and a non-empty description are required.']);
        }
        const rewritten = name === 'bash'
          ? { ...args, workdir: args.workdir === undefined ? route.remoteCwd : remotePath(args.workdir as string, route.remoteCwd) }
          : { ...args, file_path: remotePath(args.file_path as string, route.remoteCwd) };
        // The original execution identity/arguments and durable session header are never rewritten.
        await route.acquire(exec.signal);
        actor(agent, route, exec);
        const result = await original.execute(rewritten, exec);
        actor(agent, route, exec);
        return result;
      },
    });
  }

  const management = (name: string, description: string, field: string | undefined,
    operation: (args: Record<string, unknown>, exec: ToolRunContext) => unknown | Promise<unknown>) => {
    const definition: ToolDefinition = {
      name, description,
      parameters: {
        type: 'object', properties: field ? { [field]: { type: 'string', minLength: 1 } } : {},
        required: field ? [field] : [], additionalProperties: false,
      },
      output: { schema: { type: 'object', additionalProperties: true }, render: (_args, value) => [{ type: 'text', text: JSON.stringify(value) }] },
      async execute(input, exec) {
        actor(agent, route, exec, false);
        const args = validate(definition, input);
        const result = await operation(args, exec);
        actor(agent, route, exec, false);
        return result;
      },
    };
    ctx.tools.register(definition);
  };
  management('opensandbox_status', 'Report local binding facts only. Does not create a sandbox or verify remote existence.', undefined, () => route.status());
  management('opensandbox_reconnect', 'Explicitly connect this session to the supplied sandbox ID. Never creates a replacement sandbox. Pending or unconfirmed deletion must be resolved first.', 'sandbox_id',
    (args, exec) => route.reconnect(args.sandbox_id as string, exec.signal));
  management('opensandbox_delete', 'Delete the known sandbox only when confirm_sandbox_id matches its ID. Does not acquire or create an unopened sandbox.', 'confirm_sandbox_id',
    args => route.delete(args.confirm_sandbox_id as string));

  const definitions = new Map<string, ToolDefinition>();
  for (const name of NATIVE_TOOL_NAMES) {
    const definition = ctx.tools.get(name, agent);
    if (!definition) throw new Error('Native tool composition is incomplete.');
    definitions.set(name, definition);
  }
  return definitions;
}
