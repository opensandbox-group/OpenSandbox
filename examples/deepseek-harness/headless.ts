// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

import { pathToFileURL } from 'node:url';
import type {} from '@deepseek-ai/dsh-tools';
import type { AnonymousUserId } from '@deepseek-ai/dsh-anonymous-user-id';
import { DeepSeekAdapter, resolveAdapterOptions } from '@deepseek-ai/dsh-llm-deepseek';
import {
  BindingOpenError, UnknownOutcomeError, createHeadlessSession, openBinding,
} from '@opensandbox/deepseek-harness';
import type { BoundSandbox, HeadlessSession, OpenOptions } from '@opensandbox/deepseek-harness';
import { cleanupSession, summarizeRun, toolFailed } from './outcome.js';
export { cleanupSession, summarizeRun, toolFailed } from './outcome.js';

class ConfigError extends Error {}
export interface Config {
  openOptions: OpenOptions;
  cleanup: 'close' | 'kill';
  model: string;
  prompt: string;
  modelBaseURL: string;
  modelApiKey: string;
  userId: AnonymousUserId;
}
export function readConfig(env: NodeJS.ProcessEnv = process.env): Config {
  const required = (key: string) => {
    const value = env[key];
    if (!value || !value.trim() || /[\x00-\x1f\x7f]/.test(value)) throw new ConfigError(`${key} must be explicitly set without control characters.`);
    return value;
  };
  if (env.DSH_REAL_MODEL !== '1') throw new ConfigError('Set DSH_REAL_MODEL=1 only when you intend to contact the configured service and paid model.');
  const domain = required('OPEN_SANDBOX_DOMAIN'), protocol = required('OPEN_SANDBOX_PROTOCOL');
  if (!['http', 'https'].includes(protocol)) throw new ConfigError('OPEN_SANDBOX_PROTOCOL must be http or https.');
  const endpoint = new URL(`${protocol}://${domain}`);
  if (endpoint.username || endpoint.password || endpoint.pathname !== '/' || endpoint.search || endpoint.hash || domain.includes('://')) {
    throw new ConfigError('OPEN_SANDBOX_DOMAIN must contain only the explicit hostname and optional port.');
  }
  const cwd = required('DSH_REMOTE_CWD');
  if (!cwd.startsWith('/')) throw new ConfigError('DSH_REMOTE_CWD must be an existing absolute Linux directory.');
  const cleanup = required('DSH_CLEANUP');
  if (cleanup !== 'close' && cleanup !== 'kill') throw new ConfigError('DSH_CLEANUP must be close or kill.');
  const mode = required('DSH_SANDBOX_MODE');
  const base = {
    sessionId: required('DSH_SESSION_ID'), remoteCwd: cwd,
    connectionConfig: {domain, protocol: protocol as 'http' | 'https', apiKey: env.OPEN_SANDBOX_API_KEY ?? '', disableMetrics: true},
  };
  let openOptions: OpenOptions;
  if (mode === 'connect') openOptions = {...base, kind: 'connect', sandboxId: required('DSH_SANDBOX_ID')};
  else if (mode === 'create-image' || mode === 'create-template') {
    const raw = required('DSH_TTL_SECONDS'), timeoutSeconds = Number(raw);
    if (!/^[1-9][0-9]*$/.test(raw) || !Number.isSafeInteger(timeoutSeconds)) throw new ConfigError('DSH_TTL_SECONDS must be a positive safe integer.');
    openOptions = mode === 'create-image'
      ? {...base, kind: 'create-image', image: required('DSH_IMAGE'), timeoutSeconds}
      : {...base, kind: 'create-template', templateId: required('DSH_TEMPLATE_ID'), timeoutSeconds};
  } else throw new ConfigError('DSH_SANDBOX_MODE must be create-image, create-template or connect.');
  const modelBaseURL = required('DEEPSEEK_BASE_URL');
  const modelEndpoint = new URL(modelBaseURL);
  if (modelEndpoint.protocol !== 'https:' || modelEndpoint.username || modelEndpoint.password || modelEndpoint.search || modelEndpoint.hash) {
    throw new ConfigError('DEEPSEEK_BASE_URL must be an explicit HTTPS Messages root without credentials, query or fragment.');
  }
  const userId = required('DSH_USER_ID');
  if (!/^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i.test(userId)) throw new ConfigError('DSH_USER_ID must be an explicit anonymous UUID v4.');
  return {openOptions, cleanup, model: required('DSH_MODEL'), prompt: required('DSH_PROMPT'), modelBaseURL,
    modelApiKey: required('DEEPSEEK_API_KEY'), userId: userId as AnonymousUserId};
}

/** Public adapter SPI only. No stock host, telemetry, account or retry plugins are mounted. */
export function createModelAdapter(config: Config): DeepSeekAdapter {
  const options = resolveAdapterOptions({baseURL: config.modelBaseURL, maxTokens: 4096,
    models: [{id: config.model, inputModalities: ['text']}], retryPolicy: {mode: 'normal', maxRetries: 0}});
  return new DeepSeekAdapter({
    options: () => options,
    resolveAuth: async () => ({headers: {'x-api-key': config.modelApiKey}}),
    resolveUserId: () => config.userId,
    prepareExtensions: async () => ({fields: {}, accept: async () => undefined}),
  });
}

export async function main(): Promise<void> {
  const config = readConfig(); // Validate everything before any connection/model work.
  const signal = new AbortController();
  let session: HeadlessSession | undefined, binding: BoundSandbox | undefined;
  let shuttingDown = false;
  const cancel = () => {
    if (shuttingDown) return; // Keep signal listeners installed while cleanup settles.
    try { signal.abort(); session?.agent.cancel({kind: 'user'}); }
    catch { process.exitCode = 1; } // Signal callbacks must never publish raw cancellation errors.
  };
  process.on('SIGINT', cancel); process.on('SIGTERM', cancel);
  try {
    binding = await openBinding({...config.openOptions, signal: signal.signal});
    console.log(JSON.stringify({mode: 'real', descriptor: binding.descriptor}));
    session = await createHeadlessSession({binding, adapter: createModelAdapter(config), provider: 'deepseek-official', model: config.model});
    let agentErrors = 0, toolFailures = 0;
    session.agent.ctx.on('agent/error', () => { agentErrors++; });
    session.agent.ctx.on('tools/result', (_exec, result) => { if (!result.isError && toolFailed(result.value)) toolFailures++; });
    const offset = session.agent.session.snapshotEvents().length;
    if (signal.signal.aborted) throw new Error('Cancelled before turn submission.');
    await session.run(config.prompt); // Quiescence alone is not a success assertion.
    const outcome = summarizeRun(session.agent, offset, agentErrors, toolFailures);
    console.log(JSON.stringify({mode: 'real', outcome}));
    if (!outcome.success) process.exitCode = 1;
  } finally {
    shuttingDown = true; // Set before disposing: Agent.cancel is invalid after its projection is released.
    if (binding) {
      const cleanup = await cleanupSession(session, binding, config.cleanup);
      console.log(JSON.stringify({mode: 'real', cleanup}));
      if (!cleanup.disposed || !cleanup.closed || cleanup.deleted === false) process.exitCode = 1;
    }
    process.off('SIGINT', cancel); process.off('SIGTERM', cancel);
  }
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  await main().catch(error => {
    process.exitCode = 1;
    // Never print raw adapter/SDK errors, prompts, credentials or cause chains.
    if (error instanceof ConfigError) console.error(error.message);
    else if (error instanceof BindingOpenError) console.error(JSON.stringify({error: 'binding_open_failed', stage: error.stage, sandboxId: error.sandboxId, cleanupState: error.cleanupState}));
    else if (error instanceof UnknownOutcomeError) console.error(JSON.stringify({error: 'outcome_unknown', operation: error.operation, correlationId: error.correlationId}));
    else console.error('Headless example failed; no automatic retry or offline fallback was attempted.');
  });
}
