// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

import { spawnSync } from 'node:child_process';
import { chmod, mkdir, readFile, rm, writeFile } from 'node:fs/promises';
import { join } from 'node:path';
import { afterAll, beforeAll, describe, expect, it } from 'vitest';
import { HelperClient } from '../src/helper-client.js';
import { helperSdk } from './fixtures/helper-sdk.js';

// Execute the exact embedded checks captured from production, only in disposable local test fixtures.
let f: Awaited<ReturnType<typeof helperSdk>>;
let client: HelperClient;
let bootstrap: string, requestDirectory: string, verify: string[], helper: string, directory: string;
beforeAll(async () => {
  f = await helperSdk(); client = new HelperClient(f.binding); await client.ready();
  bootstrap = f.calls.find(call => call.argv[2]?.includes('prefix="opensandbox-dsh-"'))!.argv[2]!;
  requestDirectory = f.calls.find(call => call.argv[2]?.includes('prefix="request-"'))!.argv[2]!;
  verify = f.calls.find(call => call.argv[2]?.includes('hashlib.sha256(data)'))!.argv;
  helper = f.local(verify[3]!); directory = helper.slice(0, helper.lastIndexOf('/'));
});
afterAll(async () => { await client?.dispose(); await f?.cleanup(); });
const run = (optimize: string, script: string, args: string[] = []) => spawnSync('python3', ['-c', script, ...args], {
  env: { PATH: process.env.PATH, PYTHONOPTIMIZE: optimize }, encoding: 'utf8', timeout: 5_000,
});

describe('mandatory Python checks under optimization', () => {
  it.each(['0', '1', '2'])('enforces Linux and Python >=3.8 with PYTHONOPTIMIZE=%s', async optimize => {
    const valid = run(optimize, bootstrap); expect(valid.status).toBe(0);
    await rm(valid.stdout.trim(), { recursive: true, force: true });
    for (const prefix of ['import sys; sys.version_info=(3,7)\n', 'import sys; sys.platform="unsupported"\n']) {
      const invalid = run(optimize, prefix + bootstrap);
      // Remove a wrongly created directory on the RED baseline too.
      if (invalid.stdout.startsWith('/tmp/opensandbox-dsh-')) await rm(invalid.stdout.trim(), { recursive: true, force: true });
      expect(invalid.status).not.toBe(0);
    }
  });
  it.each(['0', '1', '2'])('enforces request-parent ownership and mode with PYTHONOPTIMIZE=%s', async optimize => {
    const parent = join(f.root, `parent-${optimize}`); await mkdir(parent, { mode: 0o700 });
    expect(run(optimize, requestDirectory, [parent]).status).toBe(0);
    await chmod(parent, 0o755);
    expect(run(optimize, requestDirectory, [parent]).status).not.toBe(0);
    await chmod(parent, 0o700);
    expect(run(optimize, 'import os; os.getuid=lambda: -1\n' + requestDirectory, [parent]).status).not.toBe(0);
  });
  it.each(['0', '1', '2'])('enforces helper ownership, mode, size and hash with PYTHONOPTIMIZE=%s', async optimize => {
    const path = join(f.root, `helper-${optimize}.py`); const bytes = await readFile(helper);
    const args = [path, verify[4]!, verify[5]!]; const script = verify[2]!;
    await writeFile(path, bytes, { mode: 0o600 }); expect(run(optimize, script, args).status).toBe(0);
    await chmod(path, 0o644); expect(run(optimize, script, args).status).not.toBe(0); await chmod(path, 0o600);
    expect(run(optimize, 'import os; os.getuid=lambda: -1\n' + script, args).status).not.toBe(0);
    await writeFile(path, Buffer.concat([bytes, Buffer.from('\n# valid appended comment\n')]));
    expect(run(optimize, script, args).status).not.toBe(0);
    await writeFile(path, Buffer.from(bytes.toString('utf8').replace('Copyright', 'copyright')));
    expect(run(optimize, script, args).status).not.toBe(0);
    expect(run(optimize, script, [directory, verify[4]!, verify[5]!]).status).not.toBe(0);
  });
});
