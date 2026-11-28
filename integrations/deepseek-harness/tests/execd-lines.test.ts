// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest';
import { execdLines } from './fixtures/execd-lines.js';

describe('offline execd line-event boundary', () => {
  it.each([
    { raw: 'path\n', expected: ['path'] },
    { raw: 'path\r\n', expected: ['path'] },
    { raw: 'first\rsecond\n', expected: ['first', 'second'] },
    { raw: '\n\r\n\n', expected: ['\n', '\n', '\n'] },
    { raw: 'path\n\n', expected: ['path', '\n'] },
    { raw: 'unfinished', expected: ['unfinished'] },
    { raw: '', expected: [] },
    { raw: '世界\n', expected: ['世界'] },
  ])('emits execd records for $raw across arbitrary byte chunks', ({ raw, expected }) => {
    const records: string[] = []; const lines = execdLines(text => records.push(text));
    for (const byte of Buffer.from(raw)) lines.write(new Uint8Array([byte]));
    lines.end(); expect(records).toEqual(expected);
  });
});
