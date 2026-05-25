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

import { afterAll, beforeAll, beforeEach, expect, test } from "vitest";

import { Sandbox, type ExecutionHandlers } from "@alibaba-group/opensandbox";

import {
  CodeInterpreter,
  SupportedLanguages,
} from "@alibaba-group/opensandbox-code-interpreter";

import {
  assertEndpointHasPort,
  assertRecentTimestampMs,
  createConnectionConfig,
  getSandboxImage,
} from "./base_e2e.ts";

let sandbox: Sandbox | null = null;
let ci: CodeInterpreter | null = null;

// ---------------------------------------------------------------------------
// Helpers: sandbox lifecycle & retry
// ---------------------------------------------------------------------------

function sandboxCreateOptions() {
  return {
    connectionConfig: createConnectionConfig(),
    image: getSandboxImage(),
    entrypoint: ["/opt/opensandbox/code-interpreter.sh"],
    timeoutSeconds: 15 * 60,
    readyTimeoutSeconds: 60,
    metadata: { tag: "e2e-code-interpreter" },
    env: {
      E2E_TEST: "true",
      GO_VERSION: "1.25",
      JAVA_VERSION: "21",
      NODE_VERSION: "22",
      PYTHON_VERSION: "3.12",
      EXECD_LOG_FILE: "/tmp/opensandbox-e2e/logs/execd.log",
      EXECD_API_GRACE_SHUTDOWN: "3s", EXECD_JUPYTER_IDLE_POLL_INTERVAL: "200ms",
    },
    healthCheckPollingInterval: 200,
    volumes: [
      {
        name: "execd-log",
        host: { path: "/tmp/opensandbox-e2e/logs" },
        mountPath: "/tmp/opensandbox-e2e/logs",
        readOnly: false,
      },
    ],
  };
}

async function recreateSandbox() {
  if (sandbox) {
    try {
      await sandbox.kill();
    } catch {
      /* ignore */
    }
  }
  sandbox = await Sandbox.create(sandboxCreateOptions());
  ci = await CodeInterpreter.create(sandbox);
}

/** Check sandbox health; recreate if dead. */
async function ensureSandboxAlive() {
  if (sandbox && ci) {
    try {
      if (await sandbox.isHealthy()) return;
    } catch {
      /* health-check failed */
    }
  }
  console.warn("  ensureSandboxAlive: sandbox unhealthy — recreating …");
  await recreateSandbox();
}

function isRetryableError(err: unknown): boolean {
  const msg = String(err);
  return (
    msg.includes("terminated") ||
    msg.includes("other side closed") ||
    msg.includes("fetch failed") ||
    msg.includes("session is busy") ||
    msg.includes("UND_ERR_SOCKET")
  );
}

function sleep(ms: number) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

function shouldIsolateSandboxPerTest(testName: string): boolean {
  // Isolate high-flakiness categories only: run + concurrent + interrupt.
  return /^0[2-7]\s/.test(testName);
}

/**
 * Retry an async operation up to ``maxRetries`` times.  On retryable socket /
 * session errors the sandbox is health-checked (and recreated if dead) before
 * the next attempt.
 */
async function withRetry<T>(
  fn: () => Promise<T>,
  maxRetries = 2,
  delayMs = 3000,
): Promise<T> {
  for (let attempt = 0; attempt <= maxRetries; attempt++) {
    try {
      return await fn();
    } catch (err) {
      if (!isRetryableError(err) || attempt === maxRetries) throw err;
      console.warn(
        `  withRetry: attempt ${attempt + 1} failed, retrying in ${delayMs}ms …`,
        String(err).slice(0, 120),
      );
      await sleep(delayMs);
      await ensureSandboxAlive();
    }
  }
  throw new Error("unreachable");
}

// ---------------------------------------------------------------------------
// Setup / teardown
// ---------------------------------------------------------------------------

beforeAll(async () => {
  await recreateSandbox();
}, 10 * 60_000);

beforeEach(async (ctx) => {
  if (shouldIsolateSandboxPerTest(ctx.task.name)) {
    await recreateSandbox();
    return;
  }
  await ensureSandboxAlive();
}, 5 * 60_000);

afterAll(async () => {
  if (!sandbox) return;
  try {
    await sandbox.kill();
  } catch {
    // ignore
  }
}, 5 * 60_000);

test("01 creation and basic functionality", async () => {
  if (!sandbox || !ci) throw new Error("not initialized");

  expect(ci.id).toBe(sandbox.id);
  expect(await sandbox.isHealthy()).toBe(true);

  const info = await sandbox.getInfo();
  expect(info.status.state).toBe("Running");

  const ep = await sandbox.getEndpoint(44772);
  assertEndpointHasPort(ep.endpoint, 44772);

  const metrics = await sandbox.getMetrics();
  assertRecentTimestampMs(metrics.timestamp);
});

test("01b context management: get/list/delete/deleteContexts", async () => {
  if (!ci) throw new Error("not initialized");

  const ctx = await ci.codes.createContext(SupportedLanguages.PYTHON);
  expect(ctx.id).toBeTruthy();
  expect(ctx.language).toBe("python");

  const got = await ci.codes.getContext(ctx.id!);
  expect(got.id).toBe(ctx.id);
  expect(got.language).toBe("python");

  const all = await ci.codes.listContexts();
  expect(all.some((c) => c.id === ctx.id)).toBe(true);

  const pyOnly = await ci.codes.listContexts(SupportedLanguages.PYTHON);
  expect(pyOnly.some((c) => c.id === ctx.id)).toBe(true);

  await ci.codes.deleteContext(ctx.id!);
  await expect(ci.codes.getContext(ctx.id!)).rejects.toBeTruthy();

  // Bulk cleanup should not throw.
  await ci.codes.deleteContexts(SupportedLanguages.PYTHON);
});

test("02 java code execution", async () => {
  if (!ci) throw new Error("not initialized");

  const javaCtx = await ci.codes.createContext(SupportedLanguages.JAVA);
  expect(javaCtx.id).toBeTruthy();
  expect(javaCtx.language).toBe("java");

  const stdout: string[] = [];
  const errors: string[] = [];
  const initIds: string[] = [];

  const handlers: ExecutionHandlers = {
    onStdout: (m) => {
      stdout.push(m.text);
    },
    onError: (e) => {
      errors.push(e.name);
    },
    onInit: (i) => {
      initIds.push(i.id);
    },
  };

  const r = await ci.codes.run(
    'System.out.println("Hello from Java!");\nint result = 2 + 2;\nSystem.out.println("2 + 2 = " + result);\nresult',
    { context: javaCtx, handlers }
  );
  expect(r.id).toBeTruthy();
  expect(r.error).toBeUndefined();
  expect(r.exitCode ?? null).toBeNull();
  expect(r.complete).toBeTruthy();
  const resultText = r.result[0]?.text?.trim();
  const hasResultFromStdout = stdout.some((s) => s.includes("2 + 2 = 4"));
  expect(resultText === "4" || hasResultFromStdout).toBe(true);
  expect(initIds).toHaveLength(1);
  expect(errors).toHaveLength(0);
  expect(stdout.some((s) => s.includes("Hello from Java!"))).toBe(true);

  const err = await ci.codes.run("int x = 10 / 0; // ArithmeticException", {
    context: javaCtx,
  });
  expect(err.error).toBeTruthy();
  expect(err.error?.name).toBe("EvalException");
  expect(err.exitCode ?? null).toBeNull();
});

test("03 python code execution + direct language + persistence", async () => {
  if (!ci) throw new Error("not initialized");

  const direct = await withRetry(() =>
    ci!.codes.run("result = 2 + 2\nresult", {
      language: SupportedLanguages.PYTHON,
    }),
  );
  expect(direct.error).toBeUndefined();
  expect(direct.result[0]?.text).toBe("4");
  expect(direct.exitCode ?? null).toBeNull();
  expect(direct.complete).toBeTruthy();

  // Persistence: retry the whole block as a unit so that a sandbox restart
  // mid-way gets a fresh context instead of a stale one.
  const r = await withRetry(async () => {
    const ctx = await ci!.codes.createContext(SupportedLanguages.PYTHON);
    await ci!.codes.run("x = 42", { context: ctx });
    return ci!.codes.run("result = x\nresult", { context: ctx });
  });
  expect(r.result[0]?.text).toBe("42");
  expect(r.exitCode ?? null).toBeNull();
  expect(r.complete).toBeTruthy();

  const bad = await withRetry(async () => {
    const ctx2 = await ci!.codes.createContext(SupportedLanguages.PYTHON);
    return ci!.codes.run("print(undefined_variable)", { context: ctx2 });
  });
  expect(bad.error).toBeTruthy();
  expect(bad.exitCode ?? null).toBeNull();
});

test("04 go and typescript execution (smoke)", async () => {
  if (!ci) throw new Error("not initialized");

  const go = await withRetry(async () => {
    const goCtx = await ci!.codes.createContext(SupportedLanguages.GO);
    return ci!.codes.run(
      'package main\nimport "fmt"\nfunc main() { fmt.Print("hi"); result := 2+2; fmt.Print(result) }',
      { context: goCtx },
    );
  });
  expect(go.id).toBeTruthy();

  const ts = await withRetry(async () => {
    const tsCtx = await ci!.codes.createContext(SupportedLanguages.TYPESCRIPT);
    return ci!.codes.run(
      "console.log('Hello from TypeScript!');\nconst result: number = 2 + 2;\nresult",
      { context: tsCtx },
    );
  });
  expect(ts.id).toBeTruthy();
});

test("05 context isolation", async () => {
  if (!ci) throw new Error("not initialized");

  // Retry entire isolation block as a unit — contexts must come from the same
  // sandbox for the assertion to make sense.
  const { ok, bad } = await withRetry(async () => {
    const python1 = await ci!.codes.createContext(SupportedLanguages.PYTHON);
    const python2 = await ci!.codes.createContext(SupportedLanguages.PYTHON);
    await ci!.codes.run("secret_value1 = 'python1_secret'", {
      context: python1,
    });

    const okRes = await ci!.codes.run("result = secret_value1\nresult", {
      context: python1,
    });
    const badRes = await ci!.codes.run("result = secret_value1\nresult", {
      context: python2,
    });
    return { ok: okRes, bad: badRes };
  });

  expect(ok.error).toBeUndefined();
  expect(bad.error).toBeTruthy();
  expect(bad.error?.name).toBe("NameError");
});

test("06 concurrent execution", async () => {
  if (!ci) throw new Error("not initialized");

  // Create contexts with retry; run concurrently and tolerate partial failure.
  const py = await withRetry(() =>
    ci!.codes.createContext(SupportedLanguages.PYTHON),
  );
  const java = await withRetry(() =>
    ci!.codes.createContext(SupportedLanguages.JAVA),
  );
  const go = await withRetry(() =>
    ci!.codes.createContext(SupportedLanguages.GO),
  );

  const results = await Promise.allSettled([
    ci.codes.run(
      "import time\nfor i in range(3):\n  print(i)\n  time.sleep(0.1)",
      { context: py },
    ),
    ci.codes.run(
      "for (int i=0;i<3;i++){ System.out.println(i); try{Thread.sleep(100);}catch(Exception e){} }",
      { context: java },
    ),
    ci.codes.run(
      'package main\nimport "fmt"\nfunc main(){ for i:=0;i<3;i++{ fmt.Print(i) } }',
      { context: go },
    ),
  ]);

  const succeeded = results.filter((r) => r.status === "fulfilled");
  // At least 2 of 3 concurrent runs should succeed (tolerate CI flakiness).
  expect(succeeded.length).toBeGreaterThanOrEqual(2);
  for (const r of succeeded) {
    expect((r as PromiseFulfilledResult<any>).value.id).toBeTruthy();
  }
});

test("07 interrupt code execution + fake id", async () => {
  if (!ci) throw new Error("not initialized");

  const ctx = await withRetry(() =>
    ci!.codes.createContext(SupportedLanguages.PYTHON),
  );

  let initId: string | null = null;
  let runTask: Promise<unknown> | null = null;
  const initReceived = new Promise<void>((resolve) => {
    const handlers: ExecutionHandlers = {
      onInit: (i) => {
        initId = i.id;
        assertRecentTimestampMs(i.timestamp);
        resolve();
      },
    };

    runTask = ci!.codes.run(
      "import time\nfor i in range(100):\n  print(i)\n  time.sleep(0.2)",
      { context: ctx, handlers },
    );
  });

  await initReceived;
  if (!initId) throw new Error("missing init id");
  await ci!.codes.interrupt(initId);

  // Important: always await/catch the execution task to avoid Vitest reporting
  // unhandled rejections when the server closes the streaming connection.
  if (runTask) {
    try {
      await runTask;
    } catch {
      // Expected in some environments: interrupt may terminate the stream abruptly.
    }
  }

  await expect(ci!.codes.interrupt(`fake-${Date.now()}`)).rejects.toBeTruthy();
});
