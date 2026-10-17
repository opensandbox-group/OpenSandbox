// Copyright 2026 The OpenSandbox Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

const PTY_STDIN = 0x00;
const PTY_STDOUT = 0x01;
const PTY_STDERR = 0x02;
const PTY_REPLAY = 0x03;

export function encodeStdin(text: string): Uint8Array {
  const body = new TextEncoder().encode(text);
  const frame = new Uint8Array(body.length + 1);
  frame[0] = PTY_STDIN;
  frame.set(body, 1);
  return frame;
}

/** Bytes to write to the terminal, or null when the frame is not output. */
export function decodePtyOutput(frame: Uint8Array): Uint8Array | null {
  if (frame.length === 0) return null;
  const kind = frame[0];
  if (kind === PTY_STDOUT || kind === PTY_STDERR) return frame.subarray(1);
  if (kind === PTY_REPLAY && frame.length >= 9) return frame.subarray(9);
  return null;
}
