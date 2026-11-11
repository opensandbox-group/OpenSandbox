// Copyright 2026 The OpenSandbox Authors
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
import { defineConfig } from "tsup";

const entries = ["src/index.ts", "src/internal.ts", "src/poolRedis.ts"];

export default defineConfig([
  {
    entry: entries,
    format: ["esm"],
    dts: true,
    outDir: "dist",
    clean: true,
    sourcemap: true,
    target: "es2022",
  },
  {
    entry: entries,
    format: ["cjs"],
    outDir: "dist/cjs",
    clean: false,
    sourcemap: true,
    target: "es2022",
    outExtension: () => ({ js: ".cjs" }),
  },
]);
