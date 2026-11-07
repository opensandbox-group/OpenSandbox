// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

import { mkdtempSync, mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { tmpdir } from 'node:os';
import { spawnSync } from 'node:child_process';
import { describe, expect, it } from 'vitest';

const guide = readFileSync(new URL('../../../docs/guides/deepseek-harness.md', import.meta.url), 'utf8');

describe('native DSH user journey documentation', () => {
  it('separates native installation from embedding and covers the complete native journey', () => {
    for (const text of ['@opensandbox/deepseek-harness-native', '@deepseek-ai/dsh@0.2.1-alpha.1',
      'dsh plugin --profile opensandbox add', '--from-default-profile web', '--no-open',
      'opensandbox_status', 'opensandbox_reconnect', 'sandbox_id', 'opensandbox_delete', 'confirm_sandbox_id',
      'remoteCwd', 'ttlSeconds', 'requestTimeoutSeconds', 'readyTimeoutSeconds', 'preflightTimeoutSeconds',
      'settlementTimeoutMs', 'report.txt', 'same native session', 'copy/save', 'ACK fast-sandbox',
      'No live deployment or model result', 'does not install a model adapter']) {
      expect(guide.replace(/\s+/g, ' '), text).toContain(text);
    }
    expect(guide).not.toContain('--force');
    expect(guide).not.toContain('--legacy-peer-deps');
    const embedded = readFileSync(new URL('../../../docs/examples/deepseek-harness.md', import.meta.url), 'utf8');
    expect(embedded).toContain('/guides/deepseek-harness');
    expect(embedded).toContain('private headless session');
    expect(readFileSync(new URL('../../../docs/.vitepress/config.mts', import.meta.url), 'utf8')).toContain('/guides/deepseek-harness');
  });
  it('installs the independent profile fence before the first native startup', () => {
    const init = guide.indexOf('dsh --profile opensandbox --from-default-profile web --dump-config');
    const fence = guide.indexOf('cp examples/opensandbox-web.patch.yml');
    const install = guide.indexOf('dsh plugin --profile opensandbox add');
    const start = guide.indexOf('dsh --profile opensandbox --no-open');
    expect(init).toBeGreaterThan(0); expect(init).toBeLessThan(fence);
    expect(fence).toBeLessThan(install); expect(install).toBeLessThan(start);
    expect(guide).toContain('test ! -e "$DSH_HOME/profiles/opensandbox"');
    for (const text of ['Keep the patch', 'not a sandbox for arbitrary plugins', 'gateway and client budgets',
      'does not automatically replay', 'explicit reconnect', 'not remote artifact retrieval',
      'failed-readiness/preflight', 'local SDK handles']) expect(guide.replace(/\s+/g, ' '), text).toContain(text);
  });
  it('refuses an existing profile before invoking the CLI or replacing its patch', () => {
    const root = mkdtempSync(join(tmpdir(), 'dsh-native-doc-'));
    try {
      const profile = join(root, 'profiles/opensandbox'), bin = join(root, 'bin');
      mkdirSync(profile, { recursive: true }); mkdirSync(bin);
      writeFileSync(join(profile, 'cordis.patch.yml'), 'keep existing settings');
      writeFileSync(join(bin, 'dsh'), '#!/bin/sh\necho must-not-invoke\nexit 42\n', { mode: 0o755 });
      const block = [...guide.matchAll(/```sh\n([\s\S]*?)```/g)].find(match => match[1]!.includes('--from-default-profile'))![1]!;
      const result = spawnSync('bash', ['-c', block], { encoding: 'utf8', env: { ...process.env, DSH_HOME: root, PATH: bin + ':' + process.env.PATH } });
      expect(result.status).not.toBe(0); expect(result.stdout).not.toContain('must-not-invoke');
      expect(readFileSync(join(profile, 'cordis.patch.yml'), 'utf8')).toBe('keep existing settings');
    } finally { rmSync(root, { recursive: true, force: true }); }
  });
  it('runs the documented artifact download with an offline SDK fixture and refuses overwrite', () => {
    const root = mkdtempSync(join(tmpdir(), 'dsh-native-download-'));
    try {
      const block = [...guide.matchAll(/```sh\n([\s\S]*?)```/g)].find(match => match[1]!.includes('node --input-type=module'))![1]!;
      const source = block.split("<<'JS'\n")[1]!.split('\nJS')[0]!;
      expect(spawnSync(process.execPath, ['--input-type=module', '--check'], { input: source, encoding: 'utf8' }).status).toBe(0);
      const fixture = `export class Sandbox { static async connect(o) {
        if (o.sandboxId !== 'fixture-id' || o.connectionConfig.domain !== 'offline.invalid' || o.connectionConfig.protocol !== 'https') throw new Error();
        return { files: { readBytes: async p => { if (p !== '/tmp/report.txt') throw new Error(); return new Uint8Array([0, 255, 65]); } }, close: async () => console.log('local-close') };
      } }`;
      const offline = source.replace("'@alibaba-group/opensandbox'", JSON.stringify('data:text/javascript,' + encodeURIComponent(fixture)));
      const destination = join(root, 'report.txt');
      const options = { input: offline, encoding: 'utf8' as const, env: { ...process.env, DSH_SANDBOX_ID: 'fixture-id', DSH_DOWNLOAD_PATH: destination, OPENSANDBOX_DOMAIN: 'offline.invalid' } };
      const first = spawnSync(process.execPath, ['--input-type=module'], options);
      expect(first.status).toBe(0); expect(first.stdout).toContain('local-close');
      expect([...readFileSync(destination)]).toEqual([0, 255, 65]);
      const second = spawnSync(process.execPath, ['--input-type=module'], options);
      expect(second.status).toBe(1); expect(second.stdout).toContain('local-close');
      expect(second.stderr.trim()).toBe('Artifact download failed; no automatic retry.');
      expect([...readFileSync(destination)]).toEqual([0, 255, 65]);
    } finally { rmSync(root, { recursive: true, force: true }); }
  });
  it('keeps all shell examples syntactically valid without executing installation or live calls', () => {
    const blocks = [...guide.matchAll(/```sh\n([\s\S]*?)```/g)];
    expect(blocks.length).toBeGreaterThan(3);
    for (const block of blocks) expect(spawnSync('bash', ['-n'], { input: block[1], encoding: 'utf8' }).status).toBe(0);
  });
});
