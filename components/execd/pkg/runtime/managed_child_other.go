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

//go:build !linux

package runtime

import "os/exec"

// The hardening floor and the init reaper are Linux-only; externally
// started children need no preparation on other platforms.
func PrepareManagedChild(cmd *exec.Cmd) (func(), error) { return func() {}, nil }

func AdoptManagedChild(cmd *exec.Cmd) {}
