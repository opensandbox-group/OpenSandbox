// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

import type { CollectedOutput } from '@deepseek-ai/dsh-shell';

type OutputRead = { text: string; nextOffset: number; lossy: boolean };
const continuation = (byte: number | undefined) => byte !== undefined && (byte & 0xc0) === 0x80;

/** Bounded UTF-8 tail with whole-stream byte offsets and an independent consuming cursor. */
export class OutputBuffer {
  #tail = Buffer.alloc(0);
  #total = 0;
  #cursor = 0;
  #incomplete = false;

  constructor(readonly maxBytes: number) {
    if (!Number.isSafeInteger(maxBytes) || maxBytes <= 0) throw new RangeError('Output byte budget must be a positive safe integer.');
  }

  append(text: string): void {
    const bytes = Buffer.from(text, 'utf8');
    this.#total += bytes.length;
    if (bytes.length >= this.maxBytes) {
      this.#tail = bytes.subarray(bytes.length - this.maxBytes);
    } else {
      this.#tail = Buffer.concat([this.#tail.subarray(Math.max(0, this.#tail.length + bytes.length - this.maxBytes)), bytes]);
    }
    let skip = 0;
    while (continuation(this.#tail[skip])) skip++;
    // Copy: never retain the backing allocation of an arbitrarily large event.
    this.#tail = Buffer.from(this.#tail.subarray(skip));
  }

  markIncomplete(): void { this.#incomplete = true; }

  read(offset: number, maxBytes: number = this.maxBytes): OutputRead {
    if (!Number.isSafeInteger(offset) || offset < 0 || !Number.isSafeInteger(maxBytes) || maxBytes <= 0) {
      throw new RangeError('Output offsets and budgets must be nonnegative/positive safe integers.');
    }
    const startOffset = this.#total - this.#tail.length;
    let start = Math.max(0, offset - startOffset);
    let lossy = this.#incomplete || offset < startOffset;
    while (continuation(this.#tail[start])) { start++; lossy = true; }
    let end = Math.min(this.#tail.length, start + maxBytes);
    while (end > start && continuation(this.#tail[end])) end--;
    if (start >= this.#tail.length) return { text: '', nextOffset: Math.max(offset, this.#total), lossy };
    return { text: this.#tail.subarray(start, end).toString('utf8'), nextOffset: startOffset + end, lossy };
  }

  readDelta(): OutputRead {
    const read = this.read(this.#cursor);
    this.#cursor = read.nextOffset;
    return read;
  }

  collected(): CollectedOutput {
    return { text: this.#tail.toString('utf8'), truncated: this.#incomplete || this.#total > this.#tail.length };
  }
}
