// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

import { execFile } from 'node:child_process';
import { readFile } from 'node:fs/promises';
import { promisify } from 'node:util';
import { Context } from '@deepseek-ai/cordis';
import { expect, it } from 'vitest';
import { RemoteFileSystem } from '../src/filesystem.js';
import { helperSdk } from './fixtures/helper-sdk.js';

const exec = promisify(execFile);

it('leaves only requested files to stage after write, edit and disposal in a Git worktree', async () => {
  const sdk = await helperSdk();
  const fs = new RemoteFileSystem(new Context(), { binding: sdk.binding });
  const cwd = sdk.local('/workspace');
  try {
    await exec('git', ['init', '--quiet'], { cwd });
    const target = await fs.resolve('notes/file.txt');
    const written = await fs.writeText(target, 'before', { kind: 'createIfAbsent' });
    await fs.editText(target, { oldString: 'before', newString: 'after', replaceAll: false }, { version: written.version });
    await fs.dispose();

    expect(await readFile(sdk.local('/workspace/notes/file.txt'), 'utf8')).toBe('after');
    expect(await sdk.artifactRoots()).toEqual([]);
    await exec('git', ['add', '-A'], { cwd });
    const staged = await exec('git', ['diff', '--cached', '--name-only', '-z'], { cwd });
    expect(staged.stdout.split('\0').filter(Boolean)).toEqual(['notes/file.txt']);
  } finally {
    await fs.dispose();
    await sdk.cleanup();
  }
});
