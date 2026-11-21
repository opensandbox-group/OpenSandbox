// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

export interface NativeConfig {
  readonly domain: string;
  readonly protocol: 'http' | 'https';
  readonly apiKey?: string;
  readonly kind: 'create-template' | 'create-image';
  readonly templateId?: string;
  readonly image?: string;
  readonly remoteCwd?: string;
  readonly ttlSeconds?: number;
  readonly requestTimeoutSeconds?: number;
  readonly readyTimeoutSeconds?: number;
  readonly preflightTimeoutSeconds?: number;
  readonly settlementTimeoutMs?: number;
}

export interface ResolvedNativeConfig extends NativeConfig {
  readonly remoteCwd: string;
  readonly ttlSeconds: number;
  readonly requestTimeoutSeconds: number;
  readonly readyTimeoutSeconds: number;
  readonly preflightTimeoutSeconds: number;
  readonly settlementTimeoutMs: number;
}

export class NativeConfigError extends Error {
  readonly code = 'invalid_config';
  constructor() { super('Invalid OpenSandbox native configuration.'); this.name = 'NativeConfigError'; }
}

const fields = new Set(['domain', 'protocol', 'apiKey', 'kind', 'templateId', 'image', 'remoteCwd',
  'ttlSeconds', 'requestTimeoutSeconds', 'readyTimeoutSeconds', 'preflightTimeoutSeconds', 'settlementTimeoutMs']);
const controls = /[\x00-\x1f\x7f]/;
const fail = (): never => { throw new NativeConfigError(); };
function text(value: unknown, maximum = 4096): value is string {
  return typeof value === 'string' && value.length > 0 && value.length <= maximum && value === value.trim() && !controls.test(value);
}
function positive(value: unknown, fallback: number, maximum: number, integer = false): number {
  if (value === undefined) return fallback;
  if (typeof value !== 'number' || !Number.isFinite(value) || value <= 0 || value > maximum || (integer && !Number.isSafeInteger(value))) fail();
  return value as number;
}

/** Complete, network-free validation. Error messages never include input or SDK diagnostics. */
export function validateNativeConfig(input: unknown): Readonly<ResolvedNativeConfig> {
  if (!input || typeof input !== 'object' || Array.isArray(input)) fail();
  const value = input as Record<string, unknown>;
  if (Object.keys(value).some(key => !fields.has(key))) fail();
  if (!text(value.domain, 512) || /[\s/?#@\\%]/.test(value.domain as string)) fail();
  if (value.protocol !== 'http' && value.protocol !== 'https') fail();
  // Domain is an authority, not a URL; keeping protocol separate avoids credentials/path ambiguity.
  try {
    const url = new URL(`${value.protocol}://${value.domain}`);
    if (!url.hostname || url.username || url.password || url.pathname !== '/' || (url.port && Number(url.port) < 1)) fail();
    if (!/^(?:[A-Za-z0-9.-]+|\[[0-9a-fA-F:.]+\])(?::[0-9]+)?$/.test(value.domain as string)) fail();
  } catch { fail(); }
  if (value.apiKey !== undefined && !text(value.apiKey)) fail();
  if (value.kind !== 'create-template' && value.kind !== 'create-image') fail();
  if (value.kind === 'create-template' && (!text(value.templateId, 512) || value.image !== undefined)) fail();
  if (value.kind === 'create-image' && (!text(value.image, 2048) || value.templateId !== undefined)) fail();
  const remoteCwd = value.remoteCwd ?? '/tmp';
  if (!text(remoteCwd) || !(remoteCwd as string).startsWith('/')) fail();
  return Object.freeze({
    domain: value.domain as string, protocol: value.protocol as NativeConfig['protocol'],
    ...(value.apiKey !== undefined ? { apiKey: value.apiKey as string } : {}),
    kind: value.kind as NativeConfig['kind'],
    ...(value.kind === 'create-template' ? { templateId: value.templateId as string } : { image: value.image as string }),
    remoteCwd: remoteCwd as string,
    ttlSeconds: positive(value.ttlSeconds, 600, Number.MAX_SAFE_INTEGER, true),
    requestTimeoutSeconds: positive(value.requestTimeoutSeconds, 15, 2147483.647),
    readyTimeoutSeconds: positive(value.readyTimeoutSeconds, 30, 2147483.647),
    preflightTimeoutSeconds: positive(value.preflightTimeoutSeconds, 10, 2147483.647),
    settlementTimeoutMs: positive(value.settlementTimeoutMs, 3000, 2147483647, true),
  });
}
