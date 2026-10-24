// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest';
import { OutputBuffer } from '../src/output.js';

describe('bounded UTF-8 output', () => {
  it('keeps split streams in independent byte-coordinate buffers', () => {
    const out = new OutputBuffer(100);
    const err = new OutputBuffer(100);
    out.append('hello'); err.append('warning');
    expect(out.readDelta()).toEqual({ text: 'hello', nextOffset: 5, lossy: false });
    expect(out.readDelta().text).toBe('');
    expect(err.readDelta().text).toBe('warning');
    expect(out.read(0, 100).text).toBe('hello');
    out.append('世界');
    expect(out.read(5, 3)).toEqual({ text: '世', nextOffset: 8, lossy: false });
    expect(out.read(8, 100)).toEqual({ text: '界', nextOffset: 11, lossy: false });
  });
  it('retains a bounded tail on UTF-8 boundaries and reports dropped bytes', () => {
    const out = new OutputBuffer(5);
    out.append('a世'); out.append('界z');
    expect(out.read(0, 100)).toEqual({ text: '界z', nextOffset: 8, lossy: true });
    expect(out.collected()).toEqual({ text: '界z', truncated: true });
    expect(out.readDelta().lossy).toBe(true);
    expect(out.readDelta()).toEqual({ text: '', nextOffset: 8, lossy: false });
    expect(Buffer.byteLength(out.collected().text)).toBeLessThanOrEqual(5);
  });
  it('never emits replacement characters just because a read cap cuts a character', () => {
    const out = new OutputBuffer(100); out.append('😀x');
    expect(out.read(0, 3)).toEqual({ text: '', nextOffset: 0, lossy: false });
    expect(out.read(1, 5)).toEqual({ text: 'x', nextOffset: 5, lossy: true });
    expect(out.read(0, 4)).toEqual({ text: '😀', nextOffset: 4, lossy: false });
  });
  it('independent observed reads never consume the incremental cursor', () => {
    const out = new OutputBuffer(3); out.append('abc');
    expect(out.read(0, 3).nextOffset).toBe(3);
    expect(out.readDelta().text).toBe('abc');
    out.append('def');
    expect(out.read(3, 3)).toEqual({ text: 'def', nextOffset: 6, lossy: false });
    expect(out.read(0, 3).lossy).toBe(true);
    expect(out.readDelta()).toEqual({ text: 'def', nextOffset: 6, lossy: false });
  });
  it('marks incomplete observation honestly and validates byte budgets', () => {
    const out = new OutputBuffer(3); out.append('ok'); out.markIncomplete();
    expect(out.collected()).toEqual({ text: 'ok', truncated: true });
    expect(out.read(0, 3).lossy).toBe(true);
    expect(() => new OutputBuffer(0)).toThrow();
    expect(() => out.read(-1, 2)).toThrow();
    expect(() => out.read(0, Infinity)).toThrow();
  });
});
