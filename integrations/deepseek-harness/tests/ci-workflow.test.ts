// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

import { readFileSync } from 'node:fs';
import { spawnSync } from 'node:child_process';
import { describe, expect, it } from 'vitest';

const workflowName = 'deepseek-harness-test.yml';
const workflowUrl = new URL(`../../../.github/workflows/${workflowName}`, import.meta.url);
const workflow = () => readFileSync(workflowUrl, 'utf8');
const detector = readFileSync(new URL('../../../.github/workflows/detect-changes.yml', import.meta.url), 'utf8');
const script = detector.split('          script: |\n')[1]!.split('\n').map(line => line.slice(12)).join('\n');
const AsyncFunction = Object.getPrototypeOf(async function () {}).constructor;
async function relevant(paths: { filename: string; previous_filename?: string }[], eventName = 'pull_request', area = 'deepseek-harness') {
  const result: { relevant?: string; failure?: string } = {};
  await new AsyncFunction('context', 'github', 'core', 'process', script)(
    { eventName, repo: { owner: 'offline', repo: 'fixture' }, payload: { pull_request: { number: 1 } } },
    { paginate: async () => paths, rest: { pulls: { listFiles: {} } } },
    { info() {}, setOutput: (_name: string, value: string) => { result.relevant = value; }, setFailed: (message: string) => { result.failure = message; } },
    { env: { AREA: area } },
  );
  if (result.failure) throw new Error(result.failure);
  return result.relevant;
}
const related = [
  'integrations/deepseek-harness/src/binding.ts', 'integrations/deepseek-harness/pnpm-lock.yaml',
  'examples/deepseek-harness/smoke.ts', 'examples/deepseek-harness/pnpm-lock.yaml',
  'server/opensandbox_server/services/fast_sandbox/create_mapping.py',
  'docs/examples/deepseek-harness.md', 'docs/examples/deepseek-harness-ubuntu.md', 'docs/.vitepress/config.mts',
  'scripts/fast-sandbox-env/integration-env.sh', 'manifests/third-party/fast-sandbox.commit',
  'sdks/sandbox/javascript/src/sandbox.ts', 'sdks/package.json', 'sdks/pnpm-lock.yaml',
  'specs/sandbox-lifecycle.yml', 'specs/execd-api.yaml',
  '.github/workflows/detect-changes.yml', `.github/workflows/${workflowName}`,
];
const unrelated = ['README.md', 'cli/main.py', 'sdks/sandbox/kotlin/build.gradle.kts', 'docs/examples/other.md'];

describe('DSH offline CI path coverage', () => {
  it.each(related)('runs the dedicated PR suite for %s', async path => {
    expect(await relevant([{ filename: path }])).toBe('true');
  });
  it.each(unrelated)('skips unrelated PR changes to %s', async path => {
    expect(await relevant([{ filename: path }])).toBe('false');
  });
  it('includes a renamed-away relevant path and never runs an empty PR file list', async () => {
    expect(await relevant([{ filename: 'archive/old.ts', previous_filename: related[0]! }])).toBe('true');
    expect(await relevant([])).toBe('false');
  });
  it('runs push/manual invocations without querying PR files', async () => {
    expect(await relevant([], 'push')).toBe('true');
    expect(await relevant([], 'workflow_dispatch')).toBe('true');
  });
  it('rejects an unknown detector area rather than silently skipping tests', async () => {
    await expect(relevant([], 'pull_request', 'missing-area')).rejects.toThrow('Unknown test area: missing-area');
  });
  it('keeps push path coverage aligned with the PR detector', () => {
    const block = workflow().split('    paths:\n')[1]!.split('\npermissions:')[0]!;
    const patterns = [...block.matchAll(/- "([^"]+)"/g)].map(match =>
      new RegExp('^' + match[1]!.replace(/[.+?^${}()|[\]\\]/g, '\\$&').replace(/\*\*/g, '.*') + '$'));
    for (const path of related) expect(patterns.some(pattern => pattern.test(path)), path).toBe(true);
    for (const path of unrelated) expect(patterns.some(pattern => pattern.test(path)), path).toBe(false);
  });
});

describe('DSH offline CI execution and safety', () => {
  it('runs the full strict tarball acceptance once through the integration test suite', () => {
    const text = workflow();
    const integration = text.split('      - name: Check integration and strict tarball consumer\n')[1]!
      .split('      - name: Install example dependencies\n')[0]!;
    expect(integration.match(/^\s+pnpm test$/gm)).toHaveLength(1);
    expect(text).not.toMatch(/^\s+pnpm (?:run )?pack:check$/m);
    const packageTests = readFileSync(new URL('./package.test.ts', import.meta.url), 'utf8');
    expect(packageTests.match(/execFileSync\(process\.execPath, \[fileURLToPath\(script\)\]/g)).toHaveLength(1);
    expect(packageTests).toContain('strict consumer dependency tree: PASS');
    expect(packageTests).toContain('rejects stale compiled fixture/secret modules in an actual packed tarball');
  });
  it('runs the supported frozen install/build/test/consumer order without live credentials', () => {
    const text = workflow();
    for (const value of ['ubuntu-latest', 'node-version: "24"', 'version: 11.7.0', 'python-version: "3.12"',
      'persist-credentials: false', 'contents: read', 'pull-requests: read', 'DSH_REAL_MODEL: "0"', 'DSH_LIVE_E2E: "0"',
      'pnpm run typecheck', 'pnpm run build', 'pnpm test', 'pnpm run test:helper', 'pnpm smoke']) expect(text).toContain(value);
    expect(text.match(/pnpm install --frozen-lockfile/g)).toHaveLength(2);
    expect(text.indexOf('pnpm run build')).toBeLessThan(text.indexOf('working-directory: examples/deepseek-harness'));
    expect(text).not.toMatch(/pull_request_target|secrets\.|pnpm (?:run )?(?:headless|e2e)\b|permissions: write-all/);
    expect(text).toContain("if: needs.changes.outputs.relevant == 'true'");
    expect(text).toContain("always() && github.event_name == 'pull_request'");
  });
  it.each([
    ['success', 'true', 'success', 0], ['success', 'false', 'skipped', 0],
    ['failure', 'false', 'skipped', 1], ['cancelled', 'true', 'skipped', 1],
    ['success', 'true', 'failure', 1], ['success', 'true', 'cancelled', 1], ['success', 'true', 'skipped', 1],
    ['success', 'unknown', 'skipped', 1], ['success', '', 'skipped', 1], ['success', 'false', 'success', 1],
  ] as const)('aggregates detection=%s relevance=%s tests=%s as exit %s', (changes, relevance, tests, status) => {
    const run = workflow().split('      - name: Verify required jobs\n')[1]!.split('        run: |\n')[1]!
      .split('\n').map(line => line.slice(10)).join('\n');
    const result = spawnSync('bash', ['-e', '-c', run], { env: { PATH: process.env.PATH, CHANGES_RESULT: changes, RELEVANT: relevance, TEST_RESULT: tests } });
    expect(result.status).toBe(status);
  });
});
