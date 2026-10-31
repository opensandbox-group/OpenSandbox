// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

import { mkdtemp, readFile, rm, writeFile } from 'node:fs/promises';
import { createRequire } from 'node:module';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { pathToFileURL } from 'node:url';
import { Context } from '@deepseek-ai/cordis';
import type {} from '@deepseek-ai/cordis-plugin-loader';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { apply } from '../src/client.js';

const roots: Context[] = [];
const scratch: string[] = [];
afterEach(async () => {
  await Promise.all(roots.splice(0).map(root => root.fiber.dispose()));
  await Promise.all(scratch.splice(0).map(path => rm(path, { recursive: true, force: true })));
});

describe('native browser service lifecycle', () => {
  it('unregisters fileUpload on unload and keeps retained references deny-only', async () => {
    const root = new Context(); roots.push(root);
    const client = { apply };
    const provider = await root.plugin(client);
    const upload = root.get('fileUpload') as { upload(): Promise<never>; post(): Promise<never> };
    const activate = vi.fn(), deactivate = vi.fn();
    await root.plugin({
      inject: ['fileUpload'],
      apply(ctx: Context) {
        activate();
        ctx.effect(() => () => { deactivate(); });
      },
    });
    expect(activate).toHaveBeenCalledTimes(1);
    await provider.dispose();
    expect(root.get('fileUpload')).toBeUndefined();
    expect(deactivate).toHaveBeenCalledTimes(1);
    await expect(upload.upload()).rejects.toThrow('File uploads are disabled in the OpenSandbox profile.');
    await expect(upload.post()).rejects.toThrow('File uploads are disabled in the OpenSandbox profile.');

    await root.plugin(client);
    expect(root.get('fileUpload')).toBeDefined();
    expect(activate).toHaveBeenCalledTimes(2);
  });
});

describe('native browser configuration isolation', () => {
  it('does not expose host configuration through the real alpha scanner or boot graph', async () => {
    // Use the pinned CLI's actual scanner, not a duplicated graph implementation
    // or an additional production dependency. This test needs no prebuilt dist.
    const cli = createRequire(createRequire(import.meta.url).resolve('@deepseek-ai/dsh/lib/bin.js'));
    const { ClientModuleRegistry, bootInjections } = await import(pathToFileURL(cli.resolve('@deepseek-ai/dsh-client-modules')).href);
    const manifest = JSON.parse(await readFile(new URL('../../package.json', import.meta.url), 'utf8'));
    const directory = await mkdtemp(join(tmpdir(), 'opensandbox-client-config-')); scratch.push(directory);
    await writeFile(join(directory, 'package.json'), JSON.stringify({
      name: manifest.name, type: 'module', dsh: { client: manifest.dsh.client },
      exports: { '.': './index.js', './client': './client.js' },
    }));
    await writeFile(join(directory, 'index.js'), 'export function apply() {}\n');
    // Artifact execution is covered separately by pack:check. This fixture
    // isolates the scanner's handling of a credential-bearing host row.
    await writeFile(join(directory, 'client.js'), '// Configuration-free browser artifact fixture.\n');
    const sentinel = 'SENTINEL_ONLY_NATIVE_BROWSER_CONFIG_TEST';
    const readHostConfig = vi.fn(() => ({ apiKey: sentinel, domain: 'private-host.invalid' }));
    const root = new Context(); roots.push(root);
    const entry = {
      options: { name: manifest.name, get config() { return readHostConfig(); } },
      fiber: {}, disabled: false,
      parent: { tree: { ctx: { baseUrl: pathToFileURL(join(directory, 'package.json')).href } } },
    };
    root.provide('loader', {
      entries: () => [entry],
      internal: { version: 'v2', resolveSync: () => ({ url: pathToFileURL(join(directory, 'index.js')).href }) },
    } as unknown as Context['loader']);
    await root.plugin(ClientModuleRegistry);
    const registry = root.get('clientModules') as {
      graph(): { entries: { id: string; url: string }[] };
      fetchBundle(request: Request): Promise<Response>;
    };
    const graph = registry.graph();
    expect(graph.entries).toHaveLength(1);
    expect(graph.entries[0]?.id).toBe(manifest.name);
    expect(Object.keys(graph.entries[0]!).sort()).toEqual(['id', 'immediately', 'rev', 'url']);
    expect(readHostConfig).not.toHaveBeenCalled();
    const boot = JSON.stringify(bootInjections(graph));
    expect(boot).not.toContain(sentinel);
    expect(boot).not.toContain('apiKey');
    expect(boot).not.toContain('private-host.invalid');
    const response = await registry.fetchBundle(new Request(new URL(graph.entries[0]!.url, 'http://localhost')));
    expect(response.status).toBe(200);
    const bundle = await response.text();
    expect(bundle).not.toContain(sentinel);
    expect(bundle).not.toContain('apiKey');
    expect(readHostConfig).not.toHaveBeenCalled();
  });
});
