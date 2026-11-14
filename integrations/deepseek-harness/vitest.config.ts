// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

import { defineConfig } from 'vitest/config';

// The nested native artifact has a different host-peer runtime and its own staged suite.
// Never collect its raw or generated fixtures in the embedded alpha dependency world.
export default defineConfig({ test: { include: ['tests/**/*.test.ts', 'tests/**/*.test.mjs'] } });
