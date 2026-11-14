// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

import type { Context } from '@deepseek-ai/cordis';

/** Keep the native browser's mandatory service without reading or sending files. */
export function apply(ctx: Context): void {
  const deny = (): never => { throw new Error('File uploads are disabled in the OpenSandbox profile.'); };
  ctx.provide('fileUpload', Object.freeze({
    upload: async () => deny(),
    post: async () => deny(),
  }));
}
