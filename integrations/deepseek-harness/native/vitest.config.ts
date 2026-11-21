// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

import { defineConfig } from 'vitest/config';

export default defineConfig({
  root: '.native-build',
  test: { include: ['tests/**/*.test.ts'] },
});
