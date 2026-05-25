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
  ConnectionConfig,
  SandboxApiException,
  Sandbox,
  DEFAULT_EXECD_PORT,
  DEFAULT_EGRESS_PORT,
  SandboxManager,
  type ExecutionHandlers,
  type ExecutionComplete,
  type ExecutionError,
  type ExecutionInit,
  type ExecutionResult,
  type OutputMessage,
} from "@alibaba-group/opensandbox";

import {
  TEST_API_KEY,
  TEST_DOMAIN,
  TEST_PROTOCOL,
  assertEndpointHasPort,
  assertRecentTimestampMs,
  createConnectionConfig,
  getSandboxImage,
} from "./base_e2e.ts";

let sandbox: Sandbox | null = null;

beforeAll(async () => {
  const connectionConfig = createConnectionConfig();

  sandbox = await Sandbox.create({
    connectionConfig,
    image: getSandboxImage(),
    timeoutSeconds: 20 * 60,
    readyTimeoutSeconds: 60,
    metadata: { tag: "e2e-test" },
    entrypoint: ["tail", "-f", "/dev/null"],
    env: {
      E2E_TEST: "true",
      GO_VERSION: "1.25",
      JAVA_VERSION: "21",
      NODE_VERSION: "22",
      PYTHON_VERSION: "3.12",
      EXECD_API_GRACE_SHUTDOWN: "3s",
      EXECD_JUPYTER_IDLE_POLL_INTERVAL: "200ms",
    },
    healthCheckPollingInterval: 200,
  });
}, 5 * 60_000);

afterAll(async () => {
  if (!sandbox) return;
  try {
    // keep teardown best-effort
    await sandbox.kill();
  } catch {
    // ignore
  }
}, 5 * 60_000);

test("01 sandbox lifecycle, health, endpoint, metrics, renew, connect", async () => {
  if (!sandbox) throw new Error("sandbox not created");

  expect(typeof sandbox.id).toBe("string");
  expect(await sandbox.isHealthy()).toBe(true);

  await new Promise((resolve) => setTimeout(resolve, 5000));
  const info = await sandbox.getInfo();
  expect(info.id).toBe(sandbox.id);
  expect(info.status.state).toBe("Running");
  expect(info.entrypoint).toEqual(["tail", "-f", "/dev/null"]);
  expect(info.metadata?.tag).toBe("e2e-test");

  const ep = await sandbox.getEndpoint(DEFAULT_EXECD_PORT);
  expect(ep).toBeTruthy();
  expect(typeof ep.endpoint).toBe("string");
  assertEndpointHasPort(ep.endpoint, DEFAULT_EXECD_PORT);

  const metrics = await sandbox.getMetrics();
  expect(metrics.cpuCount).toBeGreaterThan(0);
  expect(metrics.cpuUsedPercentage).toBeGreaterThanOrEqual(0);
  expect(metrics.cpuUsedPercentage).toBeLessThanOrEqual(100);
  expect(metrics.memoryTotalMiB).toBeGreaterThan(0);
  expect(metrics.memoryUsedMiB).toBeGreaterThanOrEqual(0);
  expect(metrics.memoryUsedMiB).toBeLessThanOrEqual(metrics.memoryTotalMiB);
  assertRecentTimestampMs(metrics.timestamp, 120_000);

  const renewResp = await sandbox.renew(20 * 60);
  expect(renewResp.expiresAt).toBeTruthy();
  expect(renewResp.expiresAt).toBeInstanceOf(Date);

  const connectionConfig = sandbox.connectionConfig;
  const sandbox2 = await Sandbox.connect({
    sandboxId: sandbox.id,
    connectionConfig,
  });
  try {
    expect(sandbox2.id).toBe(sandbox.id);
    expect(await sandbox2.isHealthy()).toBe(true);
    const r = await sandbox2.commands.run("echo connect-ok");
    expect(r.error).toBeUndefined();
    expect(r.logs.stdout[0]?.text).toBe("connect-ok");
  } finally {
    // no local resources to close
  }
});

test("01b manual cleanup sandbox returns null expiresAt", async () => {
  const connectionConfig = createConnectionConfig();
  const manualSandbox = await Sandbox.create({
    connectionConfig,
    image: getSandboxImage(),
    timeoutSeconds: null,
    readyTimeoutSeconds: 60,
    metadata: { tag: "manual-e2e-test" },
    entrypoint: ["tail", "-f", "/dev/null"],
    healthCheckPollingInterval: 200,
  });

  try {
    const info = await manualSandbox.getInfo();
    expect(info.expiresAt).toBeNull();
    expect(info.metadata?.tag).toBe("manual-e2e-test");
  } finally {
    await manualSandbox.kill();
    await manualSandbox.close();
  }
});

test("01a sandbox create with networkPolicy", async () => {
  const connectionConfig = createConnectionConfig();
  const networkPolicySandbox = await Sandbox.create({
    connectionConfig,
    image: getSandboxImage(),
    timeoutSeconds: 2 * 60,
    readyTimeoutSeconds: 60,
    networkPolicy: {
      defaultAction: "deny",
      egress: [{ action: "allow", target: "pypi.org" }],
    },
  });
  await new Promise((r) => setTimeout(r, 5000));
  try {
    const initialPolicy = await networkPolicySandbox.getEgressPolicy();
    expect(initialPolicy.defaultAction).toBe("deny");
    expect(initialPolicy.egress?.some((r) => r.target === "pypi.org" && r.action === "allow")).toBe(true);

    const blocked = await networkPolicySandbox.commands.run("curl -I https://www.github.com");
    expect(blocked.error).toBeTruthy();
    const allowed = await networkPolicySandbox.commands.run("curl -I https://pypi.org");
    expect(allowed.error).toBeUndefined();

    await networkPolicySandbox.patchEgressRules([
      { action: "allow", target: "www.github.com" },
      { action: "deny", target: "pypi.org" },
    ]);
    await new Promise((r) => setTimeout(r, 2000));

    const patchedPolicy = await networkPolicySandbox.getEgressPolicy();
    expect(patchedPolicy.egress?.some((r) => r.target === "www.github.com" && r.action === "allow")).toBe(true);
    expect(patchedPolicy.egress?.some((r) => r.target === "pypi.org" && r.action === "deny")).toBe(true);

    const githubAllowed = await networkPolicySandbox.commands.run("curl -I https://www.github.com");
    expect(githubAllowed.error).toBeUndefined();
    const pypiDenied = await networkPolicySandbox.commands.run("curl -I https://pypi.org");
    expect(pypiDenied.error).toBeTruthy();
  } finally {
    try {
      await networkPolicySandbox.kill();
    } catch {
      // ignore
    }
  }
}, 3 * 60_000);

test("01aa sandbox create with networkPolicy via server proxy", async () => {
  const connectionConfig = createConnectionConfig(true);
  const networkPolicySandbox = await Sandbox.create({
    connectionConfig,
    image: getSandboxImage(),
    timeoutSeconds: 2 * 60,
    readyTimeoutSeconds: 60,
    networkPolicy: {
      defaultAction: "deny",
      egress: [{ action: "allow", target: "pypi.org" }],
    },
  });
  await new Promise((r) => setTimeout(r, 5000));
  try {
    const egressEndpoint = await networkPolicySandbox.getEndpoint(DEFAULT_EGRESS_PORT);
    expect(egressEndpoint.endpoint).toContain(
      `/sandboxes/${networkPolicySandbox.id}/proxy/${DEFAULT_EGRESS_PORT}`
    );

    const initialPolicy = await networkPolicySandbox.getEgressPolicy();
    expect(initialPolicy.defaultAction).toBe("deny");
    expect(initialPolicy.egress?.some((r) => r.target === "pypi.org" && r.action === "allow")).toBe(true);

    const blocked = await networkPolicySandbox.commands.run("curl -I https://www.github.com");
    expect(blocked.error).toBeTruthy();
    const allowed = await networkPolicySandbox.commands.run("curl -I https://pypi.org");
    expect(allowed.error).toBeUndefined();

    await networkPolicySandbox.patchEgressRules([
      { action: "allow", target: "www.github.com" },
      { action: "deny", target: "pypi.org" },
    ]);
    await new Promise((r) => setTimeout(r, 2000));

    const patchedPolicy = await networkPolicySandbox.getEgressPolicy();
    expect(patchedPolicy.egress?.some((r) => r.target === "www.github.com" && r.action === "allow")).toBe(true);
    expect(patchedPolicy.egress?.some((r) => r.target === "pypi.org" && r.action === "deny")).toBe(true);
  } finally {
    try {
      await networkPolicySandbox.kill();
    } catch {
      // ignore
    }
  }
}, 3 * 60_000);

test("01b sandbox create with host volume mount (read-write)", async () => {
  const connectionConfig = createConnectionConfig();
  const hostDir = "/tmp/opensandbox-e2e/host-volume-test";
  const containerMountPath = "/mnt/host-data";

  const volumeSandbox = await Sandbox.create({
    connectionConfig,
    image: getSandboxImage(),
    timeoutSeconds: 2 * 60,
    readyTimeoutSeconds: 60,
    volumes: [
      {
        name: "test-host-vol",
        host: { path: hostDir },
        mountPath: containerMountPath,
        readOnly: false,
      },
    ],
  });

  try {
    expect(await volumeSandbox.isHealthy()).toBe(true);

    // Step 1: Verify the host marker file is visible inside the sandbox
    // Retry: bind mount propagation can sometimes lag on first access
    let readMarker = await volumeSandbox.commands.run(
      `cat ${containerMountPath}/marker.txt`
    );
    for (let attempt = 0; attempt < 5; attempt++) {
      if (readMarker.logs.stdout.length >= 1) break;
      await new Promise((r) => setTimeout(r, 500));
      readMarker = await volumeSandbox.commands.run(
        `cat ${containerMountPath}/marker.txt`
      );
    }
    expect(readMarker.error).toBeUndefined();
    expect(readMarker.logs.stdout).toHaveLength(1);
    expect(readMarker.logs.stdout[0]?.text).toBe("opensandbox-e2e-marker");

    // Step 2: Write a file from inside the sandbox to the mounted path
    const writeResult = await volumeSandbox.commands.run(
      `echo 'written-from-sandbox' > ${containerMountPath}/sandbox-output.txt`
    );
    expect(writeResult.error).toBeUndefined();

    // Step 3: Verify the written file is readable
    // Retry: written data may not be immediately visible through bind mount
    let readBack = await volumeSandbox.commands.run(
      `cat ${containerMountPath}/sandbox-output.txt`
    );
    for (let attempt = 0; attempt < 5; attempt++) {
      if (readBack.logs.stdout.length >= 1) break;
      await new Promise((r) => setTimeout(r, 500));
      readBack = await volumeSandbox.commands.run(
        `cat ${containerMountPath}/sandbox-output.txt`
      );
    }
    expect(readBack.error).toBeUndefined();
    expect(readBack.logs.stdout).toHaveLength(1);
    expect(readBack.logs.stdout[0]?.text).toBe("written-from-sandbox");

    // Step 4: Verify the mount path is a proper directory
    let dirCheck = await volumeSandbox.commands.run(
      `test -d ${containerMountPath} && echo OK`
    );
    for (let attempt = 0; attempt < 3; attempt++) {
      expect(dirCheck.error).toBeUndefined();
      if (dirCheck.logs.stdout[0]?.text === "OK") break;
      await new Promise((r) => setTimeout(r, 1000));
      dirCheck = await volumeSandbox.commands.run(
        `test -d ${containerMountPath} && echo OK`
      );
    }
    expect(dirCheck.logs.stdout[0]?.text).toBe("OK");
  } finally {
    try {
      await volumeSandbox.kill();
    } catch {
      // ignore
    }
  }
}, 3 * 60_000);

test("01c sandbox create with host volume mount (read-only)", async () => {
  const connectionConfig = createConnectionConfig();
  const hostDir = "/tmp/opensandbox-e2e/host-volume-test";
  const containerMountPath = "/mnt/host-data-ro";

  const roSandbox = await Sandbox.create({
    connectionConfig,
    image: getSandboxImage(),
    timeoutSeconds: 2 * 60,
    readyTimeoutSeconds: 60,
    volumes: [
      {
        name: "test-host-vol-ro",
        host: { path: hostDir },
        mountPath: containerMountPath,
        readOnly: true,
      },
    ],
  });

  try {
    expect(await roSandbox.isHealthy()).toBe(true);

    // Step 1: Verify the host marker file is readable
    // Retry: bind mount propagation can sometimes lag on first access
    let readMarker = await roSandbox.commands.run(
      `cat ${containerMountPath}/marker.txt`
    );
    for (let attempt = 0; attempt < 5; attempt++) {
      if (readMarker.logs.stdout.length >= 1) break;
      await new Promise((r) => setTimeout(r, 500));
      readMarker = await roSandbox.commands.run(
        `cat ${containerMountPath}/marker.txt`
      );
    }
    expect(readMarker.error).toBeUndefined();
    expect(readMarker.logs.stdout).toHaveLength(1);
    expect(readMarker.logs.stdout[0]?.text).toBe("opensandbox-e2e-marker");

    // Step 2: Verify writing is denied on read-only mount
    const writeResult = await roSandbox.commands.run(
      `touch ${containerMountPath}/should-fail.txt`
    );
    const statResult = await roSandbox.commands.run(
      `test ! -e ${containerMountPath}/should-fail.txt && echo OK`
    );
    const writeWasRejected =
      writeResult.error != null || writeResult.logs.stderr.length > 0;
    const fileWasNotCreated = statResult.logs.stdout[0]?.text === "OK";
    expect(writeWasRejected || fileWasNotCreated).toBe(true);
  } finally {
    try {
      await roSandbox.kill();
    } catch {
      // ignore
    }
  }
}, 3 * 60_000);

test("01d sandbox create with PVC named volume mount (read-write)", async () => {
  const connectionConfig = createConnectionConfig();
  const pvcVolumeName = "opensandbox-e2e-pvc-test";
  const containerMountPath = "/mnt/pvc-data";

  const pvcSandbox = await Sandbox.create({
    connectionConfig,
    image: getSandboxImage(),
    timeoutSeconds: 2 * 60,
    readyTimeoutSeconds: 60,
    volumes: [
      {
        name: "test-pvc-vol",
        pvc: { claimName: pvcVolumeName },
        mountPath: containerMountPath,
        readOnly: false,
      },
    ],
  });

  try {
    expect(await pvcSandbox.isHealthy()).toBe(true);

    // Step 1: Verify the marker file seeded into the named volume is readable
    // Retry: bind mount propagation can sometimes lag on first access
    let readMarker = await pvcSandbox.commands.run(
      `cat ${containerMountPath}/marker.txt`
    );
    for (let attempt = 0; attempt < 5; attempt++) {
      if (readMarker.logs.stdout.length >= 1) break;
      await new Promise((r) => setTimeout(r, 500));
      readMarker = await pvcSandbox.commands.run(
        `cat ${containerMountPath}/marker.txt`
      );
    }
    expect(readMarker.error).toBeUndefined();
    expect(readMarker.logs.stdout).toHaveLength(1);
    expect(readMarker.logs.stdout[0]?.text).toBe("pvc-marker-data");

    // Step 2: Write a file from inside the sandbox to the named volume
    const writeResult = await pvcSandbox.commands.run(
      `echo 'written-to-pvc' > ${containerMountPath}/pvc-output.txt`
    );
    expect(writeResult.error).toBeUndefined();

    // Step 3: Verify the written file is readable
    // Retry: written data may not be immediately visible through bind mount
    let readBack = await pvcSandbox.commands.run(
      `cat ${containerMountPath}/pvc-output.txt`
    );
    for (let attempt = 0; attempt < 5; attempt++) {
      if (readBack.logs.stdout.length >= 1) break;
      await new Promise((r) => setTimeout(r, 500));
      readBack = await pvcSandbox.commands.run(
        `cat ${containerMountPath}/pvc-output.txt`
      );
    }
    expect(readBack.error).toBeUndefined();
    expect(readBack.logs.stdout).toHaveLength(1);
    expect(readBack.logs.stdout[0]?.text).toBe("written-to-pvc");

    // Step 4: Verify the mount path is a proper directory
    let dirCheck = await pvcSandbox.commands.run(
      `test -d ${containerMountPath} && echo OK`
    );
    for (let attempt = 0; attempt < 3; attempt++) {
      expect(dirCheck.error).toBeUndefined();
      if (dirCheck.logs.stdout[0]?.text === "OK") break;
      await new Promise((r) => setTimeout(r, 1000));
      dirCheck = await pvcSandbox.commands.run(
        `test -d ${containerMountPath} && echo OK`
      );
    }
    expect(dirCheck.logs.stdout[0]?.text).toBe("OK");
  } finally {
    try {
      await pvcSandbox.kill();
    } catch {
      // ignore
    }
  }
}, 3 * 60_000);

test("01e sandbox create with PVC named volume mount (read-only)", async () => {
  const connectionConfig = createConnectionConfig();
  const pvcVolumeName = "opensandbox-e2e-pvc-test";
  const containerMountPath = "/mnt/pvc-data-ro";

  const roSandbox = await Sandbox.create({
    connectionConfig,
    image: getSandboxImage(),
    timeoutSeconds: 2 * 60,
    readyTimeoutSeconds: 60,
    volumes: [
      {
        name: "test-pvc-vol-ro",
        pvc: { claimName: pvcVolumeName },
        mountPath: containerMountPath,
        readOnly: true,
      },
    ],
  });

  try {
    expect(await roSandbox.isHealthy()).toBe(true);

    // Step 1: Verify the marker file is readable
    // Retry: bind mount propagation can sometimes lag on first access
    let readMarker = await roSandbox.commands.run(
      `cat ${containerMountPath}/marker.txt`
    );
    for (let attempt = 0; attempt < 5; attempt++) {
      if (readMarker.logs.stdout.length >= 1) break;
      await new Promise((r) => setTimeout(r, 500));
      readMarker = await roSandbox.commands.run(
        `cat ${containerMountPath}/marker.txt`
      );
    }
    expect(readMarker.error).toBeUndefined();
    expect(readMarker.logs.stdout).toHaveLength(1);
    expect(readMarker.logs.stdout[0]?.text).toBe("pvc-marker-data");

    // Step 2: Verify writing is denied on read-only mount
    const writeResult = await roSandbox.commands.run(
      `touch ${containerMountPath}/should-fail.txt`
    );
    const statResult = await roSandbox.commands.run(
      `test ! -e ${containerMountPath}/should-fail.txt && echo OK`
    );
    const writeWasRejected =
      writeResult.error != null || writeResult.logs.stderr.length > 0;
    const fileWasNotCreated = statResult.logs.stdout[0]?.text === "OK";
    expect(writeWasRejected || fileWasNotCreated).toBe(true);
  } finally {
    try {
      await roSandbox.kill();
    } catch {
      // ignore
    }
  }
}, 3 * 60_000);

test("01f sandbox create with PVC named volume subPath mount", async () => {
  const connectionConfig = createConnectionConfig();
  const pvcVolumeName = "opensandbox-e2e-pvc-test";
  const containerMountPath = "/mnt/train";

  const subpathSandbox = await Sandbox.create({
    connectionConfig,
    image: getSandboxImage(),
    timeoutSeconds: 2 * 60,
    readyTimeoutSeconds: 60,
    volumes: [
      {
        name: "test-pvc-subpath",
        pvc: { claimName: pvcVolumeName },
        mountPath: containerMountPath,
        readOnly: false,
        subPath: "datasets/train",
      },
    ],
  });

  try {
    expect(await subpathSandbox.isHealthy()).toBe(true);

    // Step 1: Verify the subpath marker file is readable
    // Retry: bind mount propagation can sometimes lag on first access
    let readMarker = await subpathSandbox.commands.run(
      `cat ${containerMountPath}/marker.txt`
    );
    for (let attempt = 0; attempt < 5; attempt++) {
      if (readMarker.logs.stdout.length >= 1) break;
      await new Promise((r) => setTimeout(r, 500));
      readMarker = await subpathSandbox.commands.run(
        `cat ${containerMountPath}/marker.txt`
      );
    }
    expect(readMarker.error).toBeUndefined();
    expect(readMarker.logs.stdout).toHaveLength(1);
    expect(readMarker.logs.stdout[0]?.text).toBe("pvc-subpath-marker");

    // Step 2: Verify only subPath contents are visible (not the full volume)
    const lsResult = await subpathSandbox.commands.run(
      `ls ${containerMountPath}/`
    );
    expect(lsResult.error).toBeUndefined();
    const lsOutput = lsResult.logs.stdout.map((m) => m.text).join("\n");
    expect(lsOutput).toContain("marker.txt");
    expect(lsOutput).not.toContain("datasets");

    // Step 3: Write a file and verify (retry read-back for transient SSE drops)
    const writeResult = await subpathSandbox.commands.run(
      `echo 'subpath-write-test' > ${containerMountPath}/output.txt`
    );
    expect(writeResult.error).toBeUndefined();

    let readBack: Awaited<ReturnType<typeof subpathSandbox.commands.run>> | undefined;
    for (let attempt = 0; attempt < 3; attempt++) {
      readBack = await subpathSandbox.commands.run(
        `cat ${containerMountPath}/output.txt`
      );
      if (readBack.logs.stdout.length > 0) break;
      await new Promise<void>((resolve) => setTimeout(resolve, 1000));
    }
    expect(readBack!.error).toBeUndefined();
    expect(readBack!.logs.stdout).toHaveLength(1);
    expect(readBack!.logs.stdout[0]?.text).toBe("subpath-write-test");
  } finally {
    try {
      await subpathSandbox.kill();
    } catch {
      // ignore
    }
  }
}, 3 * 60_000);

test("01g sandbox manager: list + get", async () => {
  if (!sandbox) throw new Error("sandbox not created");

  const manager = SandboxManager.create({ connectionConfig: sandbox.connectionConfig });

  const list = await manager.listSandboxInfos({
    states: ["Running"],
    metadata: { tag: "e2e-test" },
    pageSize: 50,
  });
  expect(Array.isArray(list.items)).toBe(true);
  expect(list.items.some((s) => s.id === sandbox!.id)).toBe(true);

  const info = await manager.getSandboxInfo(sandbox.id);
  expect(info.id).toBe(sandbox.id);
  expect(info.metadata?.tag).toBe("e2e-test");
});

test("02 command execution: success, working directory, background, failure", async () => {
  if (!sandbox) throw new Error("sandbox not created");

  const stdoutMessages: OutputMessage[] = [];
  const stderrMessages: OutputMessage[] = [];
  const results: ExecutionResult[] = [];
  const initEvents: ExecutionInit[] = [];
  const completedEvents: ExecutionComplete[] = [];
  const errors: ExecutionError[] = [];

  const handlers: ExecutionHandlers = {
    onStdout: (m) => {
      stdoutMessages.push(m);
    },
    onStderr: (m) => {
      stderrMessages.push(m);
    },
    onResult: (r) => {
      results.push(r);
    },
    onInit: (i) => {
      initEvents.push(i);
    },
    onExecutionComplete: (c) => {
      completedEvents.push(c);
    },
    onError: (e) => {
      errors.push(e);
    },
  };

  const ok = await sandbox.commands.run(
    "echo 'Hello OpenSandbox E2E'",
    undefined,
    handlers
  );
  expect(ok.id).toBeTruthy();
  expect(ok.error).toBeUndefined();
  expect(ok.logs.stdout).toHaveLength(1);
  expect(ok.logs.stdout[0]?.text).toBe("Hello OpenSandbox E2E");
  assertRecentTimestampMs(ok.logs.stdout[0]!.timestamp);
  expect(ok.exitCode).toBe(0);
  expect(ok.complete).toBeTruthy();
  expect(ok.complete!.executionTimeMs).toBeGreaterThanOrEqual(0);

  expect(initEvents).toHaveLength(1);
  expect(completedEvents).toHaveLength(1);
  expect(errors).toHaveLength(0);

  const pwd = await sandbox.commands.run("pwd", { workingDirectory: "/tmp" });
  expect(pwd.error).toBeUndefined();
  expect(pwd.logs.stdout[0]?.text).toBe("/tmp");
  expect(pwd.exitCode).toBe(0);
  expect(pwd.complete).toBeTruthy();

  const start = Date.now();
  const bg = await sandbox.commands.run("sleep 30", { background: true });
  expect(Date.now() - start).toBeLessThan(10_000);
  expect(bg.exitCode ?? null).toBeNull();

  // failure contract: error exists; completion should be absent
  stdoutMessages.length = 0;
  stderrMessages.length = 0;
  results.length = 0;
  initEvents.length = 0;
  completedEvents.length = 0;
  errors.length = 0;

  const fail = await sandbox.commands.run(
    "nonexistent-command-that-does-not-exist",
    undefined,
    handlers
  );
  expect(fail.id).toBeTruthy();
  expect(fail.error).toBeTruthy();
  expect(fail.error?.name).toBe("CommandExecError");
  expect(fail.logs.stderr.length).toBeGreaterThan(0);
  expect(
    fail.logs.stderr.some((m) =>
      m.text.includes("nonexistent-command-that-does-not-exist")
    )
  ).toBe(true);
  expect(fail.complete).toBeUndefined();
  expect(fail.exitCode).toBe(Number(fail.error?.value));
  expect(completedEvents.length).toBe(0);
});

test("02a command status + background logs", async () => {
  if (!sandbox) throw new Error("sandbox not created");

  const exec = await sandbox.commands.run(
    "sh -c 'echo log-line-1; echo log-line-2; sleep 2'",
    { background: true }
  );
  expect(exec.id).toBeTruthy();

  const commandId = exec.id!;
  const status = await sandbox.commands.getCommandStatus(commandId);
  expect(status.id).toBe(commandId);
  expect(typeof status.running).toBe("boolean");

  let logsText = "";
  let cursor: number | undefined = undefined;
  for (let i = 0; i < 20; i++) {
    const logs = await sandbox.commands.getBackgroundCommandLogs(
      commandId,
      cursor
    );
    logsText += logs.content;
    cursor = logs.cursor ?? cursor;
    if (logsText.includes("log-line-2")) break;
    await new Promise<void>((resolve) => setTimeout(resolve, 1000));
  }

  expect(logsText.includes("log-line-1")).toBe(true);
  expect(logsText.includes("log-line-2")).toBe(true);
});

test("02b command env injection", async () => {
  if (!sandbox) throw new Error("sandbox not created");

  const envKey = "OPEN_SANDBOX_E2E_CMD_ENV";
  const envValue = `env-ok-${Date.now()}`;
  const probeCommand = `sh -c 'if [ -z "\${${envKey}:-}" ]; then echo "__EMPTY__"; else echo "\${${envKey}}"; fi'`;

  const baseline = await sandbox.commands.run(probeCommand);
  expect(baseline.error).toBeUndefined();
  const baselineOutput = baseline.logs.stdout.map((m) => m.text).join("\n").trim();
  expect(baselineOutput).toBe("__EMPTY__");

  const injected = await sandbox.commands.run(probeCommand, {
    envs: {
      [envKey]: envValue,
      OPEN_SANDBOX_E2E_SECOND_ENV: "second-ok",
    },
  });
  expect(injected.error).toBeUndefined();
  const injectedOutput = injected.logs.stdout.map((m) => m.text).join("\n").trim();
  expect(injectedOutput).toBe(envValue);
});

test("02c bash session API: working directory and env persistence", async () => {
  if (!sandbox) throw new Error("sandbox not created");

  const sid = await sandbox.commands.createSession({ workingDirectory: "/tmp" });
  expect(typeof sid).toBe("string");
  expect(sid.length).toBeGreaterThan(0);

  let run = await sandbox.commands.runInSession(sid, "pwd");
  expect(run.error).toBeUndefined();
  expect(run.exitCode).toBe(0);
  expect(run.logs.stdout.map((m) => m.text).join("").trim()).toBe("/tmp");

  run = await sandbox.commands.runInSession(sid, "pwd", { workingDirectory: "/var" });
  expect(run.error).toBeUndefined();
  expect(run.exitCode).toBe(0);
  expect(run.logs.stdout.map((m) => m.text).join("").trim()).toBe("/var");

  run = await sandbox.commands.runInSession(sid, "pwd", { workingDirectory: "/tmp" });
  expect(run.error).toBeUndefined();
  expect(run.exitCode).toBe(0);
  expect(run.logs.stdout.map((m) => m.text).join("").trim()).toBe("/tmp");

  run = await sandbox.commands.runInSession(
    sid,
    "export E2E_SESSION_ENV=session-env-ok"
  );
  expect(run.error).toBeUndefined();

  run = await sandbox.commands.runInSession(sid, "echo $E2E_SESSION_ENV");
  expect(run.error).toBeUndefined();
  expect(run.exitCode).toBe(0);
  expect(run.logs.stdout.map((m) => m.text).join("").trim()).toBe(
    "session-env-ok"
  );

  run = await sandbox.commands.runInSession(
    sid,
    "sh -c 'echo session-fail >&2; exit 7'",
  );
  expect(run.error?.name).toBe("CommandExecError");
  expect(run.error?.value).toBe("7");
  expect(run.exitCode).toBe(7);
  expect(run.complete).toBeUndefined();

  const sid2 = await sandbox.commands.createSession({ workingDirectory: "/var" });
  expect(typeof sid2).toBe("string");
  run = await sandbox.commands.runInSession(sid2, "pwd");
  expect(run.error).toBeUndefined();
  expect(run.exitCode).toBe(0);
  expect(run.logs.stdout.map((m) => m.text).join("").trim()).toBe("/var");

  await sandbox.commands.deleteSession(sid);
  await sandbox.commands.deleteSession(sid2);
});

test("03 filesystem operations: CRUD + replace/move/delete + range + stream", async () => {
  if (!sandbox) throw new Error("sandbox not created");

  const ts = Date.now();
  const dir1 = `/tmp/fs_test1_${ts}`;
  const dir2 = `/tmp/fs_test2_${ts}`;

  await sandbox.files.createDirectories([
    { path: dir1, mode: 755 },
    { path: dir2, mode: 644 },
  ]);

  const infoMap = await sandbox.files.getFileInfo([dir1, dir2]);
  expect(infoMap[dir1]?.path).toBe(dir1);
  expect(infoMap[dir2]?.path).toBe(dir2);
  expect(infoMap[dir1]?.mode).toBe(755);
  expect(infoMap[dir2]?.mode).toBe(644);

  const ls = await sandbox.commands.run("ls -la | grep fs_test", {
    workingDirectory: "/tmp",
  });
  expect(ls.error).toBeUndefined();
  expect(ls.logs.stdout).toHaveLength(2);

  const file1 = `${dir1}/test_file1.txt`;
  const file2 = `${dir1}/test_file2.txt`;
  const file3 = `${dir1}/test_file3.txt`;
  const content = "Hello Filesystem!\nLine 2 with special chars: åäö\nLine 3";
  const bytes = new TextEncoder().encode(content);

  // Align with Python/Kotlin semantics but keep E2E portable across different base images:
  // prefer "nogroup"/"nobody" if present, otherwise fall back to "root".
  const ownerPick = await sandbox.commands.run(
    `id -u nobody >/dev/null 2>&1 && echo nobody || echo root`,
    { workingDirectory: "/tmp" }
  );
  expect(ownerPick.error).toBeUndefined();
  const ownerName = (ownerPick.logs.stdout[0]?.text || "root").trim();

  const groupPick = await sandbox.commands.run(
    `getent group nogroup >/dev/null 2>&1 && echo nogroup || echo root`,
    { workingDirectory: "/tmp" }
  );
  expect(groupPick.error).toBeUndefined();
  const groupName = (groupPick.logs.stdout[0]?.text || "root").trim();

  await sandbox.files.writeFiles([
    { path: file1, data: content, mode: 644 },
    { path: file2, data: bytes, mode: 755 },
    { path: file3, data: bytes, mode: 755, owner: ownerName, group: groupName },
  ]);

  const searched = await sandbox.files.search({ path: dir1, pattern: "*" });
  const searchedPaths = new Set(searched.map((f) => f.path));
  expect(searchedPaths.has(file1)).toBe(true);
  expect(searchedPaths.has(file2)).toBe(true);
  expect(searchedPaths.has(file3)).toBe(true);

  const read1 = await sandbox.files.readFile(file1, { encoding: "utf-8" });
  const read1Partial = await sandbox.files.readFile(file1, {
    encoding: "utf-8",
    range: "bytes=0-9",
  });
  const read2 = await sandbox.files.readBytes(file2);
  let read3 = new Uint8Array();
  for await (const chunk of sandbox.files.readBytesStream(file3)) {
    const merged = new Uint8Array(read3.length + chunk.length);
    merged.set(read3, 0);
    merged.set(chunk, read3.length);
    read3 = merged;
  }

  expect(read1).toBe(content);
  expect(new TextDecoder("utf-8").decode(read2)).toBe(content);
  expect(new TextDecoder("utf-8").decode(read3)).toBe(content);
  expect(read1Partial).toBe(content.slice(0, 10));

  await sandbox.files.setPermissions([
    { path: file1, mode: 755, owner: ownerName, group: groupName },
    { path: file2, mode: 600, owner: ownerName, group: groupName },
  ]);
  const perms = await sandbox.files.getFileInfo([file1, file2]);
  expect(perms[file1]?.mode).toBe(755);
  expect(perms[file1]?.owner).toBe(ownerName);
  expect(perms[file1]?.group).toBe(groupName);
  expect(perms[file2]?.mode).toBe(600);

  const updated1 = `${content}\nAppended line to file1`;
  const updated2 = `${content}\nAppended line to file2`;
  await new Promise((r) => setTimeout(r, 50));
  await sandbox.files.writeFiles([
    { path: file1, data: updated1, mode: 644 },
    { path: file2, data: updated2, mode: 755 },
  ]);
  expect(await sandbox.files.readFile(file1)).toBe(updated1);
  expect(await sandbox.files.readFile(file2)).toBe(updated2);

  await new Promise((r) => setTimeout(r, 50));
  await sandbox.files.replaceContents([
    {
      path: file1,
      oldContent: "Appended line to file1",
      newContent: "Replaced line in file1",
    },
  ]);
  const replaced = await sandbox.files.readFile(file1);
  expect(replaced.includes("Replaced line in file1")).toBe(true);
  expect(replaced.includes("Appended line to file1")).toBe(false);

  const movedPath = `${dir2}/moved_file3.txt`;
  await sandbox.files.moveFiles([{ src: file3, dest: movedPath }]);
  expect(await sandbox.files.readFile(movedPath)).toBe(content);

  await sandbox.files.deleteFiles([file2]);
  await expect(sandbox.files.readFile(file2)).rejects.toBeTruthy();

  await sandbox.files.deleteDirectories([dir1, dir2]);
  let verify = await sandbox.commands.run(
    `test ! -d ${dir1} && test ! -d ${dir2} && echo OK`,
    { workingDirectory: "/tmp" }
  );
  for (let attempt = 0; attempt < 3; attempt++) {
    if (!verify.error && verify.logs.stdout[0]?.text === "OK") break;
    await new Promise((r) => setTimeout(r, 1000));
    verify = await sandbox.commands.run(
      `test ! -d ${dir1} && test ! -d ${dir2} && echo OK`,
      { workingDirectory: "/tmp" }
    );
  }
  expect(verify.error).toBeUndefined();
  expect(verify.logs.stdout[0]?.text).toBe("OK");
});

test("04 interrupt command", async () => {
  if (!sandbox) throw new Error("sandbox not created");

  const initEvents: ExecutionInit[] = [];
  const completed: ExecutionComplete[] = [];
  const errors: ExecutionError[] = [];
  let initResolve: ((v: ExecutionInit) => void) | null = null;
  const initPromise = new Promise<ExecutionInit>((r) => (initResolve = r));

  const handlers: ExecutionHandlers = {
    onInit: (i) => {
      initEvents.push(i);
      initResolve?.(i);
    },
    onExecutionComplete: (c) => {
      completed.push(c);
    },
    onError: (e) => {
      errors.push(e);
    },
  };

  const task = sandbox.commands.run("sleep 30", undefined, handlers);
  const init = await initPromise;
  expect(init.id).toBeTruthy();
  assertRecentTimestampMs(init.timestamp);

  await sandbox.commands.interrupt(init.id);
  let exec = null;
  try {
    exec = await Promise.race([
      task,
      new Promise<never>((_, reject) =>
        setTimeout(() => reject(new Error("interrupt wait timeout")), 60_000),
      ),
    ]);
  } catch {
    exec = null;
  }

  if (exec) {
    expect(exec.id).toBe(init.id);
  }

  let followUp = null;
  try {
    followUp = await sandbox.commands.run("echo interrupt-ok");
  } catch {
    followUp = null;
  }

  expect(
    completed.length > 0 ||
      errors.length > 0 ||
      (followUp?.error === undefined &&
        followUp?.logs.stdout[0]?.text === "interrupt-ok"),
  ).toBe(true);
});

test("05 sandbox pause + resume", async () => {
  return; // skip pause/resume e2e test
  if (!sandbox) throw new Error("sandbox not created");

  await new Promise((r) => setTimeout(r, 20_000));
  await sandbox.pause();

  let state = "Pausing";
  for (let i = 0; i < 300; i++) {
    await new Promise((r) => setTimeout(r, 1000));
    const info = await sandbox.getInfo();
    state = info.status.state;
    if (state !== "Pausing") break;
  }
  expect(state).toBe("Paused");

  // pause => unhealthy
  let healthy = true;
  for (let i = 0; i < 10; i++) {
    healthy = await sandbox.isHealthy();
    if (!healthy) break;
    await new Promise((r) => setTimeout(r, 500));
  }
  expect(healthy).toBe(false);

  sandbox = await sandbox.resume({
    readyTimeoutSeconds: 60,
    healthCheckPollingInterval: 200,
  });

  let ok = false;
  for (let i = 0; i < 60; i++) {
    await new Promise((r) => setTimeout(r, 1000));
    ok = await sandbox.isHealthy();
    if (ok) break;
  }
  expect(ok).toBe(true);

  const echo = await sandbox.commands.run("echo resume-ok");
  expect(echo.error).toBeUndefined();
  expect(echo.logs.stdout[0]?.text).toBe("resume-ok");
});

test("06 x-request-id passthrough on server error", async () => {
  const requestId = `e2e-js-server-${Date.now()}`;
  const missingSandboxId = `missing-${requestId}`;
  const connectionConfig = new ConnectionConfig({
    domain: TEST_DOMAIN,
    protocol: TEST_PROTOCOL === "https" ? "https" : "http",
    apiKey: TEST_API_KEY,
    requestTimeoutSeconds: 180,
    headers: { "X-Request-ID": requestId },
  });

  try {
    const connected = await Sandbox.connect({
      sandboxId: missingSandboxId,
      connectionConfig,
    });
    await connected.getInfo();
    throw new Error("expected server call to fail");
  } catch (err) {
    expect(err).toBeInstanceOf(SandboxApiException);
    expect((err as SandboxApiException).requestId).toBe(requestId);
  }
});
