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

import assert from "node:assert/strict";
import test from "node:test";

import { HealthAdapter, createExecdClient } from "../dist/internal.js";
import { SandboxApiException } from "../dist/index.js";

test("HealthAdapter treats empty 200 ping responses as healthy", async () => {
  const health = new HealthAdapter(createExecdClient({
    baseUrl: "http://execd.test",
    async fetch(request) {
      assert.equal(new URL(request.url).pathname, "/ping");
      return new Response("", { status: 200 });
    },
  }));

  assert.equal(await health.ping(), true);
});

test("HealthAdapter still maps ping API errors", async () => {
  const health = new HealthAdapter(createExecdClient({
    baseUrl: "http://execd.test",
    async fetch() {
      return Response.json({ code: "UNAVAILABLE", message: "not ready" }, { status: 503 });
    },
  }));

  await assert.rejects(() => health.ping(), SandboxApiException);
});
