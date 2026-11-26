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
	"io"
	"runtime"
)

// Overridden at build time via -ldflags.
var (
	Version   = "dev"
	BuildTime = "unknown"
	GitCommit = "unknown"
)

func Print(w io.Writer, componentName string) {
	fmt.Fprintln(w, "=====================================================")
	fmt.Fprintf(w, " %s\n", componentName)
	fmt.Fprintln(w, "-----------------------------------------------------")
	fmt.Fprintf(w, " Version     : %s\n", Version)
	fmt.Fprintf(w, " Git Commit  : %s\n", GitCommit)
	fmt.Fprintf(w, " Build Time  : %s\n", BuildTime)
	fmt.Fprintf(w, " Go Version  : %s\n", runtime.Version())
	fmt.Fprintf(w, " Platform    : %s/%s\n", runtime.GOOS, runtime.GOARCH)
	fmt.Fprintln(w, "=====================================================")
}
