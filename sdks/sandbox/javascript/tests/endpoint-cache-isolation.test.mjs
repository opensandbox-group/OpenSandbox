// Copyright 2026 The OpenSandbox Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
// http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

import assert from "node:assert/strict";
import test from "node:test";
import { EndpointCache } from "../dist/internal.js";

test("invalidating another sandbox does not discard an in-flight cache fill", async () => {
  const cache = new EndpointCache();
  let resolve;
  let calls = 0;
  const fetcher = () => {
    calls++;
    return new Promise((done) => { resolve = done; });
  };
  const pending = cache.getOrFetch("active", 8080, false, fetcher);
  cache.invalidate("unrelated");
  const joined = cache.getOrFetch("active", 8080, false, fetcher);
  resolve({ endpoint: "active:8080", headers: { token: "test" } });
  const [first, second] = await Promise.all([pending, joined]);
  assert.equal(calls, 1);
  assert.deepEqual(first, second);
  first.headers.token = "changed";
  assert.equal(second.headers.token, "test");
  assert.deepEqual(cache.get("active", 8080, false), second);
  const cached = await cache.getOrFetch("active", 8080, false, () => {
    assert.fail("unrelated invalidation must not cause a second request");
  });
  assert.deepEqual(cached, second);
});

for (const staleFails of [false, true]) {
  test(`same-sandbox invalidation fences a stale ${staleFails ? "failure" : "success"}`, async () => {
    const cache = new EndpointCache();
    let finishOld;
    let finishNew;
    const old = cache.getOrFetch("active", 8080, false, () => new Promise((resolve, reject) => {
      finishOld = staleFails ? () => reject(new Error("old fetch failed")) : () => resolve({ endpoint: "old", headers: {} });
    }));
    const oldResult = old.catch((error) => error);
    cache.invalidate("active");
    const replacement = cache.getOrFetch("active", 8080, false, () => new Promise((resolve) => { finishNew = resolve; }));
    finishOld();
    await oldResult;
    assert.equal(cache.get("active", 8080, false), undefined);
    const joined = cache.getOrFetch("active", 8080, false, () => assert.fail("replacement fetch must remain shared"));
    finishNew({ endpoint: "fresh", headers: {} });
    await Promise.all([replacement, joined]);
    assert.equal(cache.get("active", 8080, false).endpoint, "fresh");
  });
}
