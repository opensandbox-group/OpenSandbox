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

//go:build linux && bwrap

package bwrap_test

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/alibaba/opensandbox/execd/pkg/isolation"
	"github.com/alibaba/opensandbox/execd/pkg/runtime"
)

func boolPtr(b bool) *bool { return &b }

func newRunner(t *testing.T) *runtime.IsolatedRunner {
	t.Helper()
	return newRunnerWithConfig(t, isolation.Config{
		UpperRoot:       t.TempDir(),
		UpperMaxBytes:   1 << 30,
		AllowedWritable: []string{"/tmp"},
	})
}

func newRunnerWithConfig(t *testing.T, cfg isolation.Config) *runtime.IsolatedRunner {
	t.Helper()

	ctrl := runtime.NewController("", "")
	iso := isolation.NewBwrap(cfg)
	if !iso.Available() {
		t.Skip("bwrap not available")
	}

	r, err := runtime.NewIsolatedRunner(ctrl, iso, cfg)
	require.NoError(t, err)
	return r
}

func TestMain(m *testing.M) {
	if os.Getenv("SKIP_INTEGRATION") != "" {
		os.Exit(0)
	}

	cmd := exec.Command("bwrap", "--version")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "bwrap not available, skipping: %v\n", err)
		os.Exit(0)
	}

	if os.Getuid() != 0 {
		fmt.Fprintf(os.Stderr, "requires root (use: sudo go test -tags=linux,bwrap ./pkg/runtime/bwrap_test/)\n")
		os.Exit(0)
	}

	os.Exit(m.Run())
}
