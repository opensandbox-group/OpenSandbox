import assert from "node:assert/strict";
import test from "node:test";

import { ConnectionConfig, SandboxManager } from "../dist/index.js";

function createSandboxesStub() {
  const calls = [];
  const sandboxes = {
    async listSandboxes(filter) {
      calls.push({ method: "listSandboxes", args: [filter] });
      return { items: [{ id: "sbx-1" }] };
    },
    async patchSandboxMetadata(sandboxId, patch) {
      calls.push({ method: "patchSandboxMetadata", args: [sandboxId, patch] });
      return { id: sandboxId, metadata: { team: "platform" } };
    },
    async getSandbox(sandboxId) {
      calls.push({ method: "getSandbox", args: [sandboxId] });
      return { id: sandboxId };
    },
    async deleteSandbox(sandboxId) {
      calls.push({ method: "deleteSandbox", args: [sandboxId] });
    },
    async pauseSandbox(sandboxId) {
      calls.push({ method: "pauseSandbox", args: [sandboxId] });
    },
    async resumeSandbox(sandboxId) {
      calls.push({ method: "resumeSandbox", args: [sandboxId] });
    },
    async renewSandboxExpiration(sandboxId, body) {
      calls.push({ method: "renewSandboxExpiration", args: [sandboxId, body] });
    },
    async createSnapshot(sandboxId, body) {
      calls.push({ method: "createSnapshot", args: [sandboxId, body] });
      return { id: "snap-1", sandboxId, status: { state: "Creating" }, createdAt: new Date() };
    },
    async getSnapshot(snapshotId) {
      calls.push({ method: "getSnapshot", args: [snapshotId] });
      return { id: snapshotId };
    },
    async listSnapshots(filter) {
      calls.push({ method: "listSnapshots", args: [filter] });
      return { items: [{ id: "snap-1" }] };
    },
    async deleteSnapshot(snapshotId) {
      calls.push({ method: "deleteSnapshot", args: [snapshotId] });
    },
  };
  return { sandboxes, calls };
}

test("SandboxManager delegates lifecycle operations and closes its transport", async () => {
  const { sandboxes, calls } = createSandboxesStub();
  const connectionConfig = new ConnectionConfig({ domain: "http://127.0.0.1:8080" });
  let closeCalls = 0;
  connectionConfig.closeTransport = async () => {
    closeCalls += 1;
  };
  connectionConfig.withTransportIfMissing = () => connectionConfig;

  const manager = SandboxManager.create({
    connectionConfig,
    adapterFactory: {
      createLifecycleStack() {
        return { sandboxes };
      },
    },
  });

  const list = await manager.listSandboxInfos({
    states: ["Running"],
    metadata: { team: "sdk" },
    page: 2,
    pageSize: 5,
  });
  assert.equal(list.items[0].id, "sbx-1");

  const info = await manager.getSandboxInfo("sbx-42");
  assert.equal(info.id, "sbx-42");

  const patched = await manager.patchSandboxMetadata("sbx-42", {
    team: "platform",
    old: null,
  });
  assert.equal(patched.metadata.team, "platform");

  await manager.pauseSandbox("sbx-42");
  await manager.resumeSandbox("sbx-42");
  await manager.killSandbox("sbx-42");
  await manager.renewSandbox("sbx-42", 30);
  const snapshot = await manager.createSnapshot("sbx-42", { name: "before-upgrade" });
  assert.equal(snapshot.id, "snap-1");
  const snapshots = await manager.listSnapshots({ sandboxId: "sbx-42", states: ["Ready"] });
  assert.equal(snapshots.items[0].id, "snap-1");
  const loadedSnapshot = await manager.getSnapshot("snap-1");
  assert.equal(loadedSnapshot.id, "snap-1");
  await manager.deleteSnapshot("snap-1");
  await manager.close();

  assert.deepEqual(
    calls.map((entry) => entry.method),
    [
      "listSandboxes",
      "getSandbox",
      "patchSandboxMetadata",
      "pauseSandbox",
      "resumeSandbox",
      "deleteSandbox",
      "renewSandboxExpiration",
      "createSnapshot",
      "listSnapshots",
      "getSnapshot",
      "deleteSnapshot",
    ],
  );
  assert.deepEqual(calls[0].args[0], {
    states: ["Running"],
    metadata: { team: "sdk" },
    page: 2,
    pageSize: 5,
  });
  assert.deepEqual(calls[2].args, ["sbx-42", { team: "platform", old: null }]);
  assert.equal(calls[6].args[0], "sbx-42");
  assert.ok(typeof calls[6].args[1].expiresAt === "string");
  assert.ok(Number.isFinite(Date.parse(calls[6].args[1].expiresAt)));
  assert.deepEqual(calls[7].args, ["sbx-42", { name: "before-upgrade" }]);
  assert.deepEqual(calls[8].args[0], { sandboxId: "sbx-42", states: ["Ready"] });
  assert.equal(calls[9].args[0], "snap-1");
  assert.equal(calls[10].args[0], "snap-1");
  assert.equal(closeCalls, 1);
});
