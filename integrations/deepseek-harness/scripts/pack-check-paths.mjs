// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

import assert from 'node:assert/strict';
import { lstat, realpath } from 'node:fs/promises';
import { join } from 'node:path';

export async function assertTarballInstall(scratch) {
  // Temporary roots can have symlinked ancestors (for example, /var on macOS).
  const installed = join(await realpath(scratch), 'node_modules', '@opensandbox', 'deepseek-harness');
  assert(!(await lstat(installed)).isSymbolicLink(), 'Consumer must install tarball, not a source symlink');
  assert.equal(await realpath(installed), installed, 'Consumer must install tarball, not a source symlink');
  return installed;
}
