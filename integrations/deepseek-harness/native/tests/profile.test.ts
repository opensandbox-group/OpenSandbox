// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

import { readFile } from 'node:fs/promises';
import { describe, expect, it } from 'vitest';
import { parse } from 'yaml';

type Row = { id: string; disabled?: boolean; inject?: string[]; config?: Record<string, unknown> };
const root = new URL('../../', import.meta.url);
const profile = parse(await readFile(new URL('examples/opensandbox-web.patch.yml', root), 'utf8')) as Row[];
const row = (id: string) => profile.find(value => value.id === id);

describe('persistent dedicated Web profile fence', () => {
  it('inserts the native row through the actual bundle patch grammar', async () => {
    const patch = await readFile(new URL('cordis.patch.yml', root), 'utf8');
    expect(patch).toMatch(/^- insert:\n  - id: opensandbox-native\n/m);
    expect(patch).not.toContain('headless');
  });

  it('requires native readiness and presets independently of bundle activation', () => {
    for (const id of ['agent-loop', 'session-controller', 'webserver']) {
      expect(row(id)?.inject).toEqual(expect.arrayContaining(['opensandboxNativeReady', 'agentPresets']));
    }
    expect(row('webserver')?.inject).toContain('webStartup');
    expect(row('agent-preset-registry')?.config).toEqual({default: 'opensandbox', selectedDefault: 'opensandbox'});
    expect(row('session-controller')?.config).toEqual({nativeOpen: false});
    expect(row('workspace-controller')?.config?.documentsDirectory).toBe("ctx.get('profileContext').dir + '/workspace-metadata'");
    expect(new Set(profile.map(value => value.id)).size).toBe(profile.length);
  });

  it('disables stock presets and unverified host execution and artifact surfaces', () => {
    for (const id of [
      'hmr', 'client-hmr', 'subprocess', 'sandbox', 'bash-sandbox', 'pwsh-sandbox', 'fs-sandbox',
      'jobs', 'sandbox-policy', 'permission', 'ui-permission', 'tool-bash', 'tool-pwsh', 'tool-jobs', 'tool-fs', 'tool-fs-search', 'tool-plugin-manager',
      'tool-subagent', 'tool-subagent-fork', 'tool-subagent-control', 'tool-subagent-list-agents',
      'ptc-runtime', 'workflow-ptc', 'tool-workflow', 'tool-ralph', 'skill-filesystem', 'tool-skill',
      'agent-instructions', 'spill-local', 'spill-policy', 'fs-observation-policy',
      'preset-standard', 'preset-ptc', 'preset-minimal', 'preset-cordis',
      'workspace-files', 'workspace-changes', 'file-reference-local', 'file-upload', 'attachment-local',
      'terminal-controller', 'job-controller', 'office-to-pdf', 'directory-picker', 'open-in-app',
      'ui-open-in-app', 'ui-sidebar-documentpreview', 'ui-sidebar-files', 'ui-sidebar-terminal',
    ]) expect(row(id)?.disabled, id).toBe(true);
  });

  it('publishes a native browser face without re-enabling host upload storage', async () => {
    const manifest = JSON.parse(await readFile(new URL('package.json', root), 'utf8'));
    expect(manifest.dsh.client).toEqual({platform: 'web', immediately: true});
    expect(manifest.exports['./client']).toEqual({types: './dist/client.d.ts', default: './dist/client.js'});
    expect(row('file-upload')?.disabled).toBe(true);
    expect(row('attachment-local')?.disabled).toBe(true);
  });

  it('retains native session and model settings instead of replacing Agent or model configuration', () => {
    for (const id of ['agent', 'agent-default-model', 'session', 'session-persistence-jsonl', 'llm',
      'settings', 'config-editor', 'ui-settings-models', 'ui-model-selection', 'session-controller']) {
      expect(row(id)?.disabled, id).not.toBe(true);
    }
    expect(profile.some(value => value.id === 'headless-runner')).toBe(false);
  });
});
