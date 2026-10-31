// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

// OFFLINE mutation checks for the source-contract fixture, never a live server.
import { spawnSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { describe, expect, it } from 'vitest';

const serverSource = new URL('../../../server/opensandbox_server/services/fast_sandbox/create_mapping.py', import.meta.url);
const fixture = fileURLToPath(new URL('./fixtures/server-metadata-contract.py', import.meta.url));
const source = readFileSync(serverSource, 'utf8');
const metadata = JSON.stringify({ 'dsh-binding-request': '9e8aef4c-1ed7-4b85-a824-ac56c0546daa' });
const optimizationModes = ['0', '1', '2'] as const;
function run(sourcePath: string, optimize: string) {
  return spawnSync('python3', [fixture, sourcePath], {
    input: metadata, encoding: 'utf8', timeout: 10_000,
    env: { ...process.env, PYTHONOPTIMIZE: optimize },
  });
}
function withoutTemplateValidator() {
  const start = source.indexOf('def map_template_create_request(');
  expect(start).toBeGreaterThan(0);
  const template = source.slice(start), call = '    _validate_metadata(create.metadata)\n';
  expect(template).toContain(call);
  return source.slice(0, start) + template.replace(call, '');
}
function withoutRequiredConstant() {
  const declaration = '_MAX_LABEL_LENGTH = 63\n';
  expect(source).toContain(declaration);
  return source.replace(declaration, '');
}

describe('metadata source extraction survives Python optimization', () => {
  it.each(optimizationModes)('accepts the actual complete source with PYTHONOPTIMIZE=%s', optimize => {
    const result = run(fileURLToPath(serverSource), optimize);
    expect(result.error).toBeUndefined();
    expect(result.status, result.stderr).toBe(0);
    expect(JSON.parse(result.stdout)).toEqual({
      accepted: true, sourceSha256: createHash('sha256').update(source).digest('hex'),
      mappings: ['map_create_request', 'map_template_create_request'],
    });
  });
  for (const mutation of [
    { name: 'removed template validator call', source: withoutTemplateValidator, reason: 'map_template_create_request no longer calls the validator' },
    { name: 'removed required constant', source: withoutRequiredConstant, reason: 'Validator extraction changed' },
  ]) {
    it.each(optimizationModes)(`rejects ${mutation.name} with PYTHONOPTIMIZE=%s`, optimize => {
      const root = mkdtempSync(join(tmpdir(), 'dsh-metadata-source-mutation-'));
      try {
        const path = join(root, 'create_mapping.py'); writeFileSync(path, mutation.source());
        const result = run(path, optimize);
        expect(result.error).toBeUndefined();
        expect(result.status, result.stdout).toBe(1);
        expect(result.stderr).toContain(mutation.reason);
        expect(result.stdout).toBe('');
      } finally { rmSync(root, { recursive: true, force: true }); }
    });
  }
});
