// Copyright 2026 Alibaba Group Holding Ltd.
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

import { afterAll, beforeAll, expect, test } from "vitest";

import {
  Sandbox,
  SandboxManager,
} from "@alibaba-group/opensandbox";

import {
  createConnectionConfig,
  getSandboxImage,
} from "./base_e2e.ts";

let manager: SandboxManager | null = null;
let tag: string | null = null;
let s1: Sandbox | null = null;
let s2: Sandbox | null = null;
let s3: Sandbox | null = null;

function sleep(ms: number): Promise<void> {
  return new Promise((r) => setTimeout(r, ms));
}

async function waitForState(
  sandboxId: string,
  expectedState: string,
  timeoutMs = 180_000
): Promise<void> {
  if (!manager) throw new Error("sandbox manager not initialized");
  const deadline = Date.now() + timeoutMs;
  let lastState = "unknown";

  while (Date.now() < deadline) {
    const info = await manager.getSandboxInfo(sandboxId);
    lastState = info.status.state;
    if (lastState === expectedState) return;
    await sleep(1000);
  }

  throw new Error(
    `Timed out waiting for state=${expectedState}, lastState=${lastState}`
  );
}

beforeAll(async () => {
  const connectionConfig = createConnectionConfig();
  manager = SandboxManager.create({ connectionConfig });
  tag = `e2e-sandbox-manager-${Math.random().toString(16).slice(2, 10)}`;

  const common = {
    connectionConfig,
    image: getSandboxImage(),
    timeoutSeconds: 5 * 60,
    readyTimeoutSeconds: 60,
    healthCheckPollingInterval: 500,
    resource: { cpu: "1", memory: "2Gi" },
  };

  s1 = await Sandbox.create({
    ...common,
    metadata: { tag, team: "t1", env: "prod" },
    env: { E2E_TEST: "true", CASE: "mgr-s1", EXECD_API_GRACE_SHUTDOWN: "3s",
      EXECD_JUPYTER_IDLE_POLL_INTERVAL: "200ms" },
  });
  s2 = await Sandbox.create({
    ...common,
    metadata: { tag, team: "t1", env: "dev" },
    env: { E2E_TEST: "true", CASE: "mgr-s2", EXECD_API_GRACE_SHUTDOWN: "3s",
      EXECD_JUPYTER_IDLE_POLL_INTERVAL: "200ms" },
  });
  s3 = await Sandbox.create({
    ...common,
    metadata: { tag, env: "prod" },
    env: { E2E_TEST: "true", CASE: "mgr-s3", EXECD_API_GRACE_SHUTDOWN: "3s",
      EXECD_JUPYTER_IDLE_POLL_INTERVAL: "200ms" },
  });

  expect(await s1.isHealthy()).toBe(true);
  expect(await s2.isHealthy()).toBe(true);
  expect(await s3.isHealthy()).toBe(true);

  await manager.pauseSandbox(s3.id);
  await waitForState(s3.id, "Paused");
}, 10 * 60_000);

afterAll(async () => {
  for (const sbx of [s1, s2, s3]) {
    if (!sbx) continue;
    try {
      await sbx.kill();
    } catch {
      // ignore
    }
  }
}, 5 * 60_000);

test("01 states filter uses OR semantics", async () => {
  if (!manager || !tag || !s1 || !s2 || !s3) {
    throw new Error("sandbox manager not initialized");
  }

  const allStates = await manager.listSandboxInfos({
    states: ["Running", "Paused"],
    metadata: { tag },
    pageSize: 50,
  });
  const allIds = new Set(allStates.items.map((info) => info.id));
  expect(allIds.has(s1.id)).toBe(true);
  expect(allIds.has(s2.id)).toBe(true);
  expect(allIds.has(s3.id)).toBe(true);

  const pausedOnly = await manager.listSandboxInfos({
    states: ["Paused"],
    metadata: { tag },
    pageSize: 50,
  });
  const pausedIds = new Set(pausedOnly.items.map((info) => info.id));
  expect(pausedIds.has(s3.id)).toBe(true);
  expect(pausedIds.has(s1.id)).toBe(false);
  expect(pausedIds.has(s2.id)).toBe(false);

  const runningOnly = await manager.listSandboxInfos({
    states: ["Running"],
    metadata: { tag },
    pageSize: 50,
  });
  const runningIds = new Set(runningOnly.items.map((info) => info.id));
  expect(runningIds.has(s1.id)).toBe(true);
  expect(runningIds.has(s2.id)).toBe(true);
  expect(runningIds.has(s3.id)).toBe(false);
}, 2 * 60_000);

test("02 metadata filter uses AND semantics", async () => {
  if (!manager || !tag || !s1 || !s2 || !s3) {
    throw new Error("sandbox manager not initialized");
  }

  const tagAndTeam = await manager.listSandboxInfos({
    metadata: { tag, team: "t1" },
    pageSize: 50,
  });
  const tagAndTeamIds = new Set(tagAndTeam.items.map((info) => info.id));
  expect(tagAndTeamIds.has(s1.id)).toBe(true);
  expect(tagAndTeamIds.has(s2.id)).toBe(true);
  expect(tagAndTeamIds.has(s3.id)).toBe(false);

  const tagTeamEnv = await manager.listSandboxInfos({
    metadata: { tag, team: "t1", env: "prod" },
    pageSize: 50,
  });
  const tagTeamEnvIds = new Set(tagTeamEnv.items.map((info) => info.id));
  expect(tagTeamEnvIds.has(s1.id)).toBe(true);
  expect(tagTeamEnvIds.has(s2.id)).toBe(false);
  expect(tagTeamEnvIds.has(s3.id)).toBe(false);

  const tagEnv = await manager.listSandboxInfos({
    metadata: { tag, env: "prod" },
    pageSize: 50,
  });
  const tagEnvIds = new Set(tagEnv.items.map((info) => info.id));
  expect(tagEnvIds.has(s1.id)).toBe(true);
  expect(tagEnvIds.has(s3.id)).toBe(true);
  expect(tagEnvIds.has(s2.id)).toBe(false);

  const noneMatch = await manager.listSandboxInfos({
    metadata: { tag, team: "t2" },
    pageSize: 50,
  });
  const noneMatchIds = new Set(noneMatch.items.map((info) => info.id));
  expect(noneMatchIds.has(s1.id)).toBe(false);
  expect(noneMatchIds.has(s2.id)).toBe(false);
  expect(noneMatchIds.has(s3.id)).toBe(false);

  const patched = await manager.patchSandboxMetadata(s2.id, {
    env: "stage",
    team: null,
  });
  expect(patched.metadata?.env).toBe("stage");
  expect(patched.metadata?.team).toBeUndefined();

  const refreshed = await manager.getSandboxInfo(s2.id);
  expect(refreshed.metadata?.env).toBe("stage");
  expect(refreshed.metadata?.team).toBeUndefined();
}, 2 * 60_000);

test("03 invalid operations reject", async () => {
  if (!manager) throw new Error("sandbox manager not initialized");
  const nonExistentId = `non-existent-${Date.now()}`;

  await expect(manager.getSandboxInfo(nonExistentId)).rejects.toBeTruthy();
  await expect(manager.pauseSandbox(nonExistentId)).rejects.toBeTruthy();
  await expect(manager.resumeSandbox(nonExistentId)).rejects.toBeTruthy();
  await expect(manager.killSandbox(nonExistentId)).rejects.toBeTruthy();
  await expect(manager.renewSandbox(nonExistentId, 5 * 60)).rejects.toBeTruthy();
}, 60_000);
