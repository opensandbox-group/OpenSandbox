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

import { ConnectionConfig } from "../dist/index.js";

test("ConnectionConfig strips trailing slash suffix without regex backtracking", () => {
  const connectionConfig = new ConnectionConfig({
    domain: `https://api.opensandbox.test${"/".repeat(4096)}`,
  });

  assert.equal(connectionConfig.getBaseUrl(), "https://api.opensandbox.test/v1");
});

test("ConnectionConfig preserves path prefix while normalizing v1 suffix", () => {
  const connectionConfig = new ConnectionConfig({
    domain: "https://api.opensandbox.test/proxy/v1/",
  });

  assert.equal(connectionConfig.getBaseUrl(), "https://api.opensandbox.test/proxy/v1");
});
