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

import path from "node:path";
import { fileURLToPath } from "node:url";
import { createBaseConfig } from "../../eslint.base.mjs";

const __dirname = path.dirname(fileURLToPath(import.meta.url));

export default createBaseConfig({
  tsconfigRootDir: __dirname,
  tsconfigPath: "./tsconfig.json",
  extraIgnores: ["src/api/**", "src/**/*.d.ts", "src/**/*.js"],
  includeScripts: true,
});