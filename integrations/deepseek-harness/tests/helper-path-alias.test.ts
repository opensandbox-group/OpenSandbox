// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

import { mkdir, mkdtemp, readFile, realpath, rm, symlink } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterAll, beforeAll, expect, it, vi } from 'vitest';
import { HelperClient } from '../src/helper-client.js';
import { helperSdk } from './fixtures/helper-sdk.js';

// This file's isolated module mock leaves process.env and other test workers alone.
vi.mock('node:os', async importOriginal => {
  const original = await importOriginal<typeof import('node:os')>();
  return { ...original, tmpdir: vi.fn(original.tmpdir) };
});

let parent: string;
beforeAll(async () => {
  parent = await realpath(await mkdtemp(join(tmpdir(), 'dsh-helper-path-alias-')));
  await mkdir(join(parent, 'physical'));
  await symlink('physical', join(parent, 'alias'));
  vi.mocked(tmpdir).mockReturnValue(join(parent, 'alias'));
});
afterAll(async () => {
  vi.mocked(tmpdir).mockReset();
  if (parent) await rm(parent, { recursive: true, force: true });
});

it('keeps helper paths in the remote namespace when the host temp directory has a symlink ancestor', async () => {
  const f = await helperSdk();
  const client = new HelperClient(f.binding);
  try {
    await client.ready();
    const target = await client.request('resolve', { path: 'note.txt', cwd: '/workspace' });
    expect(target).toEqual({ path: '/workspace/note.txt', displayPath: '/workspace/note.txt' });
    await client.request('write-text', { target, content: 'through the alias' });
    expect(await readFile(f.local('/workspace/note.txt'), 'utf8')).toBe('through the alias');
    expect(await client.request('read-text', { target })).toMatchObject({ text: 'through the alias' });
    await client.dispose();
    expect(await f.artifactRoots()).toEqual([]);
  } finally {
    await client.dispose();
    await f.cleanup();
  }
});
