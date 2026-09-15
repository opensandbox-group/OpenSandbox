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

import type { components } from "../../src/api/execd.js";
import type { ExecdCommands } from "../../src/services/execdCommands.js";

type CommandSummary = components["schemas"]["CommandSummary"];

declare const summary: CommandSummary;

const legacyCommands: ExecdCommands = {
  runStream: async function* () {},
  run: async () => ({ logs: { stdout: [], stderr: [] }, result: [] }),
  interrupt: async () => {},
  getCommandStatus: async () => ({ id: "id", content: "", running: false }),
  getBackgroundCommandLogs: async () => ({ content: "" }),
  createSession: async () => "session",
  runInSession: async () => ({ logs: { stdout: [], stderr: [] }, result: [] }),
  deleteSession: async () => {},
};
void legacyCommands;

if (summary.running) {
  const running: true = summary.running;
  void running;
} else {
  const running: false = summary.running;
  const finishedAt: string = summary.finished_at;
  const exitCode: number | null = summary.exit_code;
  void running;
  void finishedAt;
  void exitCode;
}
