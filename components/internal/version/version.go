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

package version

import (
	"fmt"
	"runtime"
)

// Package values are typically overridden at build time via -ldflags.
var (
	// Version is the component version.
	Version = "dirty"
	// BuildTime is when the binary was built.
	BuildTime = "assigned-at-build-time"
	// GitCommit is the commit id used to build the binary.
	GitCommit = "assigned-at-build-time"
)

// EchoVersion prints build info for the given component name (e.g. "OpenSandbox Ingress", "OpenSandbox Execd").
// All components can use this by passing their display name.
func EchoVersion(componentName string) {
	fmt.Println("=====================================================")
	fmt.Printf(" %s\n", componentName)
	fmt.Println("-----------------------------------------------------")
	fmt.Printf(" Version     : %s\n", Version)
	fmt.Printf(" Git Commit  : %s\n", GitCommit)
	fmt.Printf(" Build Time  : %s\n", BuildTime)
	fmt.Printf(" Go Version  : %s\n", runtime.Version())
	fmt.Printf(" Platform    : %s/%s\n", runtime.GOOS, runtime.GOARCH)
	fmt.Println("=====================================================")
}
