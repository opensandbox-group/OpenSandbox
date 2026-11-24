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

package cli

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCLIProfileValidateAndSuites(t *testing.T) {
	var out bytes.Buffer
	cmd := NewRootCmd()
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"profile", "validate"})
	require.NoError(t, cmd.Execute())
	require.Contains(t, out.String(), "fast-sandbox-overlaybd-ref")
	require.Contains(t, out.String(), "is valid")

	out.Reset()
	cmd = NewRootCmd()
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"suites"})
	require.NoError(t, cmd.Execute())
	for _, expected := range []string{
		"schedule-burst",
		"steady-churn",
		"pause-resume",
		"rootfs-only-restore",
		"wake-storm",
		"high-density-soak",
	} {
		require.Contains(t, out.String(), expected)
	}
}
