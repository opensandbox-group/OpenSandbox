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

// Package clone3compat installs an optional seccomp rule so clone3(2) returns ENOSYS and
// libc / the Go runtime fall back to clone(2), following the same idea as
// https://github.com/AkihiroSuda/clone3-workaround .
//
// Enable on Linux via environment when starting execd:
//
//	EXECD_CLONE3_COMPAT=1       — install the filter at process start (after Go runtime init).
//	EXECD_CLONE3_COMPAT=reexec  — install the filter then re-exec the same binary so all
//	                              package init code runs with the filter already active
//	                              (closest to wrapping with the external clone3-workaround binary).
//
// Disabled when unset or empty, or set to 0, false, off, no.
package clone3compat
