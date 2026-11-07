// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

import { StringDecoder } from 'node:string_decoder';

/** Test-only model of execd commandOutputTail: nonempty lines omit CR/LF; blank lines emit LF. */
export function execdLines(emit: (text: string) => void) {
  const decoder = new StringDecoder('utf8'); let pending = ''; let lastWasCR = false;
  const consume = (text: string) => {
    for (const char of text) {
      if (char === '\n' || char === '\r') {
        if (pending) { emit(pending); pending = ''; }
        else if (!(char === '\n' && lastWasCR)) emit('\n');
        lastWasCR = char === '\r';
      } else { lastWasCR = false; pending += char; }
    }
  };
  return {
    write(bytes: Uint8Array) { consume(decoder.write(Buffer.from(bytes))); },
    end() { consume(decoder.end()); if (pending) { emit(pending); pending = ''; } },
  };
}
