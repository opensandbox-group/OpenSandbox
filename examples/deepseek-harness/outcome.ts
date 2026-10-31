// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

import type { Agent } from '@deepseek-ai/dsh-agent';
import type { BoundSandbox, HeadlessSession } from '@opensandbox/deepseek-harness';

export function toolFailed(value: unknown): boolean {
  if (!value || typeof value !== 'object') return false;
  const result = value as {kind?: unknown; exitCode?: unknown; timedOut?: unknown};
  return result.kind === 'foreground' && (result.exitCode !== 0 || result.timedOut === true);
}

export function summarizeRun(agent: Pick<Agent, 'session'>, offset: number, agentErrors: number, toolFailures: number) {
  const events = agent.session.snapshotEvents().slice(offset);
  const turns = events.filter(event => event.type === 'turn/end');
  const terminal = turns.at(-1);
  const kind = terminal?.data.reason.kind;
  // Closed output labels never echo plugin-defined reason strings or diagnostic content.
  const reason = kind === 'completed' ? 'completed' : kind === undefined ? 'missing' :
    ['error', 'aborted', 'blocked', 'max-tokens', 'interrupted', 'forked'].includes(kind) ? kind : 'other';
  const toolResults = events.filter(event => event.type === 'tool/result');
  const toolErrors = toolResults.filter(event => event.data.message.isError).length;
  return {success: turns.length === 1 && reason === 'completed' && !agentErrors && !toolErrors && !toolFailures,
    reason, turns: turns.length, toolResults: toolResults.length, toolErrors, agentErrors, toolFailures};
}

export async function cleanupSession(session: Pick<HeadlessSession, 'dispose'> | undefined,
  binding: Pick<BoundSandbox, 'kill' | 'close'>, mode: 'close' | 'kill') {
  let disposed = true, deleted: boolean | null = null, closed = true;
  try { await session?.dispose(); } catch { disposed = false; }
  if (mode === 'kill') { try { await binding.kill(); deleted = true; } catch { deleted = false; } }
  try { await binding.close(); } catch { closed = false; }
  return {disposed, deleted, closed};
}
