// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest';
import { validateNativeConfig } from '../src/config.js';

const input = { domain: 'sandbox.example:8080', protocol: 'https', kind: 'create-template', templateId: 'test-template' };

describe('native local configuration', () => {
  it('requires an explicit creation choice and snapshots all defaults', () => {
    const config = validateNativeConfig(input);
    expect(config).toMatchObject({ remoteCwd: '/tmp', ttlSeconds: 600, requestTimeoutSeconds: 15,
      readyTimeoutSeconds: 30, preflightTimeoutSeconds: 10, settlementTimeoutMs: 3000 });
    expect(Object.isFrozen(config)).toBe(true);
    expect(config).not.toBe(input);
    expect(validateNativeConfig({ ...input, kind: 'create-image', templateId: undefined, image: 'example/image:tag' }).image).toBe('example/image:tag');
  });

  it.each([
    {}, { ...input, domain: '' }, { ...input, domain: 'https://user:secret@example.test' },
    { ...input, domain: 'example.test/path' }, { ...input, domain: 'example.test?key=secret' },
    { ...input, domain: 'example.test:65536' }, { ...input, protocol: 'ftp' },
    { ...input, remoteCwd: 'relative' }, { ...input, remoteCwd: '/tmp\nsecret' },
    { ...input, kind: 'create-image' }, { ...input, templateId: '' },
    { ...input, image: 'also-selected' }, { ...input, apiKey: 'line\nbreak' },
    { ...input, ttlSeconds: 0 }, { ...input, ttlSeconds: NaN },
    { ...input, requestTimeoutSeconds: Infinity }, { ...input, readyTimeoutSeconds: -1 },
    { ...input, preflightTimeoutSeconds: 2147484 }, { ...input, settlementTimeoutMs: 0.1 },
    { ...input, unexpected: 'secret' }, null, [], 'secret',
  ])('rejects invalid config without echoing any input', invalid => {
    expect(() => validateNativeConfig(invalid)).toThrow('Invalid OpenSandbox native configuration.');
    try { validateNativeConfig(invalid); } catch (error) {
      expect(String(error)).not.toContain('secret');
      expect(JSON.stringify(error)).not.toContain('sandbox.example');
    }
  });

  it('keeps literal cwd traversal and permits bracketed IPv6 authorities', () => {
    expect(validateNativeConfig({ ...input, remoteCwd: '/tmp/link/../work', domain: '[::1]:8080' }).remoteCwd).toBe('/tmp/link/../work');
  });
});
