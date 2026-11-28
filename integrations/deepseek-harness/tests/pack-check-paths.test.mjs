// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

import { mkdirSync, mkdtempSync, realpathSync, rmSync, symlinkSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { assertTarballInstall } from '../scripts/pack-check-paths.mjs';

describe('isolated tarball consumer paths', () => {
  let root;
  beforeEach(() => {
    root = realpathSync(mkdtempSync(join(tmpdir(), 'dsh-pack-paths-')));
  });
  afterEach(() => {
    rmSync(root, {recursive: true, force: true});
  });

  it('accepts an installed directory through a symlinked temporary parent', async () => {
    const parent = join(root, 'actual');
    const alias = join(root, 'alias');
    const scratch = join(parent, 'consumer');
    const installed = join(scratch, 'node_modules', '@opensandbox', 'deepseek-harness');
    mkdirSync(installed, {recursive: true});
    symlinkSync(parent, alias, 'dir');

    await expect(assertTarballInstall(join(alias, 'consumer'))).resolves.toBe(installed);
  });

  it('rejects an installed package that is a source symlink', async () => {
    const source = join(root, 'source');
    const scratch = join(root, 'consumer');
    const scope = join(scratch, 'node_modules', '@opensandbox');
    mkdirSync(source);
    mkdirSync(scope, {recursive: true});
    symlinkSync(source, join(scope, 'deepseek-harness'), 'dir');

    await expect(assertTarballInstall(scratch)).rejects.toThrow('Consumer must install tarball, not a source symlink');
  });

  it('rejects redirected package ancestors inside the consumer', async () => {
    const redirected = join(root, 'redirected');
    const scratch = join(root, 'consumer');
    mkdirSync(join(redirected, '@opensandbox', 'deepseek-harness'), {recursive: true});
    mkdirSync(scratch);
    symlinkSync(redirected, join(scratch, 'node_modules'), 'dir');

    await expect(assertTarballInstall(scratch)).rejects.toThrow('Consumer must install tarball, not a source symlink');
  });
});
