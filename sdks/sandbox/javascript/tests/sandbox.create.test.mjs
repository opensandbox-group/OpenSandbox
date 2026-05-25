import assert from "node:assert/strict";
import test from "node:test";

import {
  ConnectionConfig,
  DEFAULT_EGRESS_PORT,
  DEFAULT_EXECD_PORT,
  DEFAULT_TIMEOUT_SECONDS,
  Sandbox,
} from "../dist/index.js";

function createAdapterFactory() {
  const recordedRequests = [];
  const endpointCalls = [];
  const egressStackCalls = [];
  const egressService = {
    async getPolicy() {
      return {
        defaultAction: "deny",
        egress: [{ action: "allow", target: "pypi.org" }],
      };
    },
    async patchRules() {},
  };
  const sandboxes = {
    async createSandbox(req) {
      recordedRequests.push(req);
      return { id: "sandbox-test-id", expiresAt: null };
    },
    async getSandbox() {
      throw new Error("not implemented");
    },
    async listSandboxes() {
      throw new Error("not implemented");
    },
    async deleteSandbox() {},
    async pauseSandbox() {},
    async resumeSandbox() {},
    async renewSandboxExpiration() {
      throw new Error("not implemented");
    },
    async getSandboxEndpoint(_sandboxId, port) {
      endpointCalls.push(port);
      return { endpoint: `127.0.0.1:${port}`, headers: { "x-port": String(port) } };
    },
  };

  const adapterFactory = {
    createLifecycleStack() {
      return { sandboxes };
    },
    createExecdStack() {
      return {
        commands: {},
        files: {},
        health: {},
        metrics: {},
      };
    },
    createEgressStack(opts) {
      egressStackCalls.push(opts);
      return { egress: egressService };
    },
  };

  return { adapterFactory, recordedRequests, endpointCalls, egressStackCalls };
}

test("Sandbox.create omits timeout when timeoutSeconds is null", async () => {
  const { adapterFactory, recordedRequests } = createAdapterFactory();

  await Sandbox.create({
    adapterFactory,
    connectionConfig: { domain: "http://127.0.0.1:8080" },
    image: "python:3.12",
    timeoutSeconds: null,
    skipHealthCheck: true,
  });

  assert.equal(recordedRequests.length, 1);
  assert.ok(!Object.hasOwn(recordedRequests[0], "timeout"));
});

test("Sandbox.create forwards secureAccess", async () => {
  const { adapterFactory, recordedRequests } = createAdapterFactory();

  await Sandbox.create({
    adapterFactory,
    connectionConfig: { domain: "http://127.0.0.1:8080" },
    image: "python:3.12",
    secureAccess: true,
    skipHealthCheck: true,
  });

  assert.equal(recordedRequests.length, 1);
  assert.equal(recordedRequests[0].secureAccess, true);
});

test("Sandbox.create forwards windows platform values", async () => {
  const { adapterFactory, recordedRequests } = createAdapterFactory();

  await Sandbox.create({
    adapterFactory,
    connectionConfig: { domain: "http://127.0.0.1:8080" },
    image: "python:3.12",
    platform: { os: "windows", arch: "amd64" },
    skipHealthCheck: true,
  });

  assert.equal(recordedRequests.length, 1);
  assert.deepEqual(recordedRequests[0].platform, { os: "windows", arch: "amd64" });
});

test("Sandbox.create floors finite timeoutSeconds", async () => {
  const { adapterFactory, recordedRequests } = createAdapterFactory();

  await Sandbox.create({
    adapterFactory,
    connectionConfig: { domain: "http://127.0.0.1:8080" },
    image: "python:3.12",
    timeoutSeconds: 61.9,
    skipHealthCheck: true,
  });

  assert.equal(recordedRequests.length, 1);
  assert.equal(recordedRequests[0].timeout, 61);
});

test("Sandbox.create uses the default timeout when timeoutSeconds is undefined", async () => {
  const { adapterFactory, recordedRequests } = createAdapterFactory();

  await Sandbox.create({
    adapterFactory,
    connectionConfig: { domain: "http://127.0.0.1:8080" },
    image: "python:3.12",
    skipHealthCheck: true,
  });

  assert.equal(recordedRequests.length, 1);
  assert.equal(recordedRequests[0].timeout, DEFAULT_TIMEOUT_SECONDS);
});

test("Sandbox.create rejects non-finite timeoutSeconds", async () => {
  for (const timeoutSeconds of [Number.NaN, Number.POSITIVE_INFINITY, Number.NEGATIVE_INFINITY]) {
    const { adapterFactory } = createAdapterFactory();
    await assert.rejects(
      Sandbox.create({
        adapterFactory,
        connectionConfig: { domain: "http://127.0.0.1:8080" },
        image: "python:3.12",
        timeoutSeconds,
        skipHealthCheck: true,
      }),
      /timeoutSeconds must be a finite number/
    );
  }
});

test("Sandbox.create restores from snapshot without entrypoint", async () => {
  const { adapterFactory, recordedRequests } = createAdapterFactory();

  await Sandbox.create({
    adapterFactory,
    connectionConfig: { domain: "http://127.0.0.1:8080" },
    snapshotId: "snap-123",
    skipHealthCheck: true,
  });

  assert.equal(recordedRequests.length, 1);
  assert.equal(recordedRequests[0].snapshotId, "snap-123");
  assert.equal(recordedRequests[0].image, undefined);
  assert.deepEqual(recordedRequests[0].entrypoint, ["tail", "-f", "/dev/null"]);
});

test("Sandbox.create restores from snapshot with explicit entrypoint", async () => {
  const { adapterFactory, recordedRequests } = createAdapterFactory();

  await Sandbox.create({
    adapterFactory,
    connectionConfig: { domain: "http://127.0.0.1:8080" },
    snapshotId: "snap-123",
    entrypoint: ["python", "app.py"],
    skipHealthCheck: true,
  });

  assert.equal(recordedRequests.length, 1);
  assert.equal(recordedRequests[0].snapshotId, "snap-123");
  assert.deepEqual(recordedRequests[0].entrypoint, ["python", "app.py"]);
});

test("Sandbox.create requires exactly one startup source", async () => {
  const { adapterFactory } = createAdapterFactory();

  await assert.rejects(
    Sandbox.create({
      adapterFactory,
      connectionConfig: { domain: "http://127.0.0.1:8080" },
      skipHealthCheck: true,
    }),
    /Exactly one of image or snapshotId must be provided/
  );

  await assert.rejects(
    Sandbox.create({
      adapterFactory,
      connectionConfig: { domain: "http://127.0.0.1:8080" },
      image: "python:3.12",
      snapshotId: "snap-123",
      skipHealthCheck: true,
    }),
    /Exactly one of image or snapshotId must be provided/
  );
});

test("Sandbox creates and reuses egress service during sandbox lifecycle", async () => {
  const { adapterFactory, endpointCalls, egressStackCalls } = createAdapterFactory();

  const sandbox = await Sandbox.create({
    adapterFactory,
    connectionConfig: { domain: "http://127.0.0.1:8080" },
    image: "python:3.12",
    skipHealthCheck: true,
  });

  await sandbox.getEgressPolicy();
  await sandbox.patchEgressRules([{ action: "allow", target: "www.github.com" }]);

  assert.deepEqual(endpointCalls, [DEFAULT_EXECD_PORT, DEFAULT_EGRESS_PORT]);
  assert.equal(egressStackCalls.length, 1);
  assert.equal(egressStackCalls[0].egressBaseUrl, `http://127.0.0.1:${DEFAULT_EGRESS_PORT}`);
  assert.deepEqual(egressStackCalls[0].endpointHeaders, { "x-port": String(DEFAULT_EGRESS_PORT) });
});

test("Sandbox.create passes OSSFS volume to request", async () => {
  const { adapterFactory, recordedRequests } = createAdapterFactory();

  await Sandbox.create({
    adapterFactory,
    connectionConfig: { domain: "http://127.0.0.1:8080" },
    image: "python:3.12",
    skipHealthCheck: true,
    volumes: [
      {
        name: "oss-data",
        ossfs: {
          bucket: "my-bucket",
          endpoint: "oss-cn-hangzhou.aliyuncs.com",
          version: "2.0",
          accessKeyId: "ak-id",
          accessKeySecret: "ak-secret",
        },
        mountPath: "/data",
        readOnly: false,
      },
    ],
  });

  assert.equal(recordedRequests.length, 1);
  assert.equal(recordedRequests[0].volumes.length, 1);
  assert.equal(recordedRequests[0].volumes[0].name, "oss-data");
  assert.equal(recordedRequests[0].volumes[0].ossfs.bucket, "my-bucket");
  assert.equal(recordedRequests[0].volumes[0].ossfs.endpoint, "oss-cn-hangzhou.aliyuncs.com");
});

test("Sandbox.create rejects volume with no backend", async () => {
  const { adapterFactory } = createAdapterFactory();

  await assert.rejects(
    Sandbox.create({
      adapterFactory,
      connectionConfig: { domain: "http://127.0.0.1:8080" },
      image: "python:3.12",
      skipHealthCheck: true,
      volumes: [{ name: "empty", mountPath: "/mnt" }],
    }),
    /must specify exactly one backend \(host, pvc, ossfs\)/
  );
});

test("Sandbox.create rejects volume with multiple backends", async () => {
  const { adapterFactory } = createAdapterFactory();

  await assert.rejects(
    Sandbox.create({
      adapterFactory,
      connectionConfig: { domain: "http://127.0.0.1:8080" },
      image: "python:3.12",
      skipHealthCheck: true,
      volumes: [
        {
          name: "conflicting",
          host: { path: "/tmp" },
          ossfs: {
            bucket: "b",
            endpoint: "e",
            accessKeyId: "id",
            accessKeySecret: "secret",
          },
          mountPath: "/mnt",
        },
      ],
    }),
    /must specify exactly one backend \(host, pvc, ossfs\)/
  );
});

test("Sandbox.create accepts host volume with windows drive path", async () => {
  const { adapterFactory, recordedRequests } = createAdapterFactory();

  await Sandbox.create({
    adapterFactory,
    connectionConfig: { domain: "http://127.0.0.1:8080" },
    image: "python:3.12",
    skipHealthCheck: true,
    volumes: [{ name: "host-vol", host: { path: "D:/sandbox-mnt/ReMe" }, mountPath: "/mnt" }],
  });

  assert.equal(recordedRequests.length, 1);
  assert.equal(recordedRequests[0].volumes[0].host.path, "D:/sandbox-mnt/ReMe");
});

test("Sandbox.create rejects host volume with relative path", async () => {
  const { adapterFactory } = createAdapterFactory();

  await assert.rejects(
    Sandbox.create({
      adapterFactory,
      connectionConfig: { domain: "http://127.0.0.1:8080" },
      image: "python:3.12",
      skipHealthCheck: true,
      volumes: [{ name: "host-vol", host: { path: "relative/path" }, mountPath: "/mnt" }],
    }),
    /Host path must be an absolute path starting with '\/' or a Windows drive letter/
  );
});

test("Sandbox.create validates host path before transport initialization", async () => {
  const { adapterFactory } = createAdapterFactory();
  const connectionConfig = new ConnectionConfig({ domain: "http://127.0.0.1:8080" });
  let transportInitialized = false;
  connectionConfig.withTransportIfMissing = () => {
    transportInitialized = true;
    throw new Error("transport initialized");
  };

  await assert.rejects(
    Sandbox.create({
      adapterFactory,
      connectionConfig,
      image: "python:3.12",
      skipHealthCheck: true,
      volumes: [{ name: "host-vol", host: { path: "relative/path" }, mountPath: "/mnt" }],
    }),
    /Host path must be an absolute path starting with '\/' or a Windows drive letter/
  );
  assert.equal(transportInitialized, false);
});

test("Sandbox.create treats null backends as absent", async () => {
  const { adapterFactory, recordedRequests } = createAdapterFactory();

  await Sandbox.create({
    adapterFactory,
    connectionConfig: { domain: "http://127.0.0.1:8080" },
    image: "python:3.12",
    skipHealthCheck: true,
    volumes: [
      {
        name: "host-with-null-ossfs",
        host: { path: "/tmp" },
        ossfs: null,
        pvc: undefined,
        mountPath: "/mnt",
      },
    ],
  });

  assert.equal(recordedRequests.length, 1);
  assert.equal(recordedRequests[0].volumes[0].host.path, "/tmp");
});
