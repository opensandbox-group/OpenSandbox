// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

import { execFileSync } from 'node:child_process';
import { cpSync, existsSync, mkdtempSync, readFileSync, rmSync, symlinkSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { describe, expect, it } from 'vitest';

const root = fileURLToPath(new URL('../', import.meta.url));
const example = new URL('../../../examples/deepseek-harness/', import.meta.url);
const manifest = JSON.parse(readFileSync(new URL('../package.json', import.meta.url), 'utf8'));

describe('installable package and exported consumers', () => {
  it('exports compiled lifecycle, providers and headless modules', () => {
    for (const name of ['.', './binding', './sdk-transport', './shell', './filesystem', './headless']) {
      expect(manifest.exports[name], name).toEqual({
        types: name === '.' ? './dist/index.d.ts' : `./dist/${name.slice(2)}.d.ts`,
        import: name === '.' ? './dist/index.js' : `./dist/${name.slice(2)}.js`,
      });
    }
  });
  it('packs, installs, imports and typechecks an isolated tarball consumer', () => {
    const script = new URL('../scripts/pack-check.mjs', import.meta.url);
    expect(existsSync(script), 'actual tarball consumer checker must exist').toBe(true);
    expect(manifest.scripts['pack:check']).toBe('pnpm run build && node scripts/pack-check.mjs');
    execFileSync(process.execPath, [fileURLToPath(new URL('../scripts/build.mjs', import.meta.url))], {cwd: root});
    const output = execFileSync(process.execPath, [fileURLToPath(script)], {
      cwd: root, encoding: 'utf8', timeout: 180_000, maxBuffer: 8 * 1024 * 1024,
    });
    expect(output).toContain('isolated tarball import, helper asset and TypeScript consumer: PASS');
    expect(output).toContain('tarball allowlist: PASS');
  }, 180_000);
  it('rejects stale compiled fixture/secret modules in an actual packed tarball', () => {
    const scratch = mkdtempSync(join(tmpdir(), 'dsh-pack-negative-')), copy = join(scratch, 'package');
    try {
      cpSync(root, copy, {recursive: true, filter: path => !path.startsWith(join(root, 'node_modules')) && !path.startsWith(join(root, 'tests'))});
      for (const name of ['fixture.js', 'fixture.d.ts', 'secret.js', 'secret.d.ts']) {
        writeFileSync(join(copy, 'dist', name), '// Harmless stale artifact sentinel.\n');
      }
      const result = spawnSync(process.execPath, [join(copy, 'scripts', 'pack-check.mjs')], {cwd: copy, encoding: 'utf8', timeout: 180_000});
      expect(result.status).not.toBe(0);
      expect(result.stderr).toContain('Unexpected tarball files');
      expect(result.stderr).toContain('dist/fixture.js');
      expect(result.stderr).toContain('dist/secret.js');
      expect(result.stdout).not.toContain('tarball allowlist: PASS');
    } finally {rmSync(scratch, {recursive: true, force: true});}
  }, 180_000);
  it('cleans obsolete generated output before compiling/prepacking a source checkout', () => {
    const scratch = mkdtempSync(join(tmpdir(), 'dsh-build-negative-')), copy = join(scratch, 'package');
    try {
      cpSync(root, copy, {recursive: true, filter: path => !path.startsWith(join(root, 'node_modules')) && !path.startsWith(join(root, 'tests'))});
      symlinkSync(join(root, 'node_modules'), join(copy, 'node_modules'), 'dir');
      writeFileSync(join(copy, 'dist', 'secret.js'), '// Harmless stale artifact sentinel.\n');
      expect(manifest.scripts.build).toBe('node scripts/build.mjs');
      expect(manifest.scripts.prepack).toBe('pnpm run build');
      const result = spawnSync(process.execPath, [join(copy, 'scripts', 'build.mjs')], {cwd: copy, encoding: 'utf8', timeout: 30_000});
      expect(result.status, result.stderr).toBe(0);
      expect(existsSync(join(copy, 'dist', 'secret.js'))).toBe(false);
      expect(existsSync(join(copy, 'dist', 'index.js'))).toBe(true);
      expect(existsSync(join(copy, 'dist', 'index.d.ts'))).toBe(true);
    } finally {rmSync(scratch, {recursive: true, force: true});}
  }, 30_000);
  it('pins direct dependencies and consumes package exports in both example modes', () => {
    const path = new URL('package.json', example);
    expect(existsSync(path), 'installable consumer example must exist').toBe(true);
    const config = JSON.parse(readFileSync(path, 'utf8'));
    expect(config.dependencies['@opensandbox/deepseek-harness']).toBe('file:../../integrations/deepseek-harness');
    for (const [name, version] of Object.entries({ ...config.dependencies, ...config.devDependencies })) {
      if (name !== '@opensandbox/deepseek-harness') expect(version).toMatch(/^\d+\.\d+\.\d+(?:-[a-z0-9.-]+)?$/);
    }
    for (const name of ['smoke.ts', 'headless.ts']) {
      const source = readFileSync(new URL(name, example), 'utf8');
      expect(source).toContain("from '@opensandbox/deepseek-harness'");
      expect(source).not.toMatch(/(?:integrations.*\/src|\.\.\/.*\/src)/);
    }
    expect(existsSync(new URL('pnpm-lock.yaml', example))).toBe(true);
  });
  it('documents contract-only smoke and opt-in real deployment separately', () => {
    const path = new URL('../../../docs/examples/deepseek-harness.md', import.meta.url);
    expect(existsSync(path), 'canonical example guide must exist').toBe(true);
    const source = readFileSync(path, 'utf8');
    expect(source).toMatch(/contract-only/);
    expect(source).toContain('DSH_REAL_MODEL');
    expect(source).toContain('quiescence');
  });
});
