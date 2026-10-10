// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it, vi } from 'vitest';
import { smoke } from '../smoke.js';

describe('offline contract-only consumer', () => {
  it('runs the actual Agent bash/read/edit loop and both binary SDK download paths without network', async () => {
    const fetch = vi.spyOn(globalThis, 'fetch').mockRejectedValue(new Error('Network is forbidden during smoke'));
    try {
      expect(await smoke()).toMatchObject({mode: 'contract-only', deployed: false, modelCalls: 0,
        tools: ['bash', 'read', 'edit'], binaryBytes: 8, outcome: {success: true, reason: 'completed', toolResults: 3}});
      expect(fetch).not.toHaveBeenCalled();
    } finally {fetch.mockRestore();}
  }, 30_000);
});
