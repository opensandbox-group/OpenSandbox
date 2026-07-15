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

export interface IsolatedWorkspaceSpec {
  path: string;
  mode?: "rw" | "overlay" | "ro";
}

export interface EnvPassthroughSpec {
  mode?: "allow" | "deny";
  keys?: string[];
}

export interface BindMount {
  source: string;
  dest?: string;
  readonly?: boolean;
}

export interface CreateIsolatedSessionRequest {
  workspace: IsolatedWorkspaceSpec;
  profile?: "strict" | "balanced";
  extra_writable?: string[];
  binds?: BindMount[];
  share_net?: boolean;
  env_passthrough?: EnvPassthroughSpec;
  uid?: number;
  gid?: number;
  uid_mode?: "setpriv" | "userns";
  idle_timeout_seconds?: number;
}

export interface IsolatedSessionInfo {
  session_id: string;
  created_at: string;
}

export interface IsolatedSessionState {
  status: "active" | "destroyed";
  created_at?: string;
  last_run_at?: string;
  idle_remaining_seconds?: number | null;
}

export interface IsolatedRunOpts {
  envs?: Record<string, string>;
  timeout_seconds?: number;
}

export interface IsolatedCapabilities {
  available: boolean;
  isolator?: string;
  version?: string;
  message?: string;
  commit_supported: boolean;
  diff_supported: boolean;
}

export interface IsolatedSessionSummary {
  session_id: string;
  status: string;
  created_at?: string;
  last_run_at?: string;
  idle_remaining_seconds?: number;
}

export interface ListIsolatedSessionsResponse {
  sessions?: IsolatedSessionSummary[];
}
