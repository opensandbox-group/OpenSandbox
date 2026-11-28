// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

import { execFileSync } from 'node:child_process';
import { rm } from 'node:fs/promises';
import { createRequire } from 'node:module';
import { fileURLToPath } from 'node:url';

// dist is regenerable output. Always remove stale modules before build/prepack.
const root = fileURLToPath(new URL('../', import.meta.url));
await rm(new URL('../dist/', import.meta.url), {recursive: true, force: true});
execFileSync(process.execPath, [createRequire(import.meta.url).resolve('typescript/bin/tsc'), '-p', 'tsconfig.build.json'],
  {cwd: root, stdio: 'inherit'});
