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

package main

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/stretchr/testify/require"
)

func TestRevisionRuntimeStartupConfiguration(t *testing.T) {
	for _, item := range []struct {
		name, profile, live, extras string
		wantError                   bool
	}{
		{"live-extra", "", "true", "8443", true},
		{"live-extras", "", "true", "8080,8443", true},
		{"live-unset", "", "true", "", false},
		{"live-empty", "", "true", "", false},
		{"live-whitespace", "", "true", "  ", false},
		{"live-invalid", "", "true", "not-a-port", true},
		{"live-reserved", "", "true", "443", true},
		{"nonlive-extra", "", "", "8443", false},
		{"nonlive-false-extra", "", "false", "8443", false},
		{"nonlive-invalid-still-validated-by-launcher", "", "", "bad", false},
		{"fast-live", constants.ProfileFastSandbox, "true", "", true},
		{"fast-live-extra", constants.ProfileFastSandbox, "true", "8443", true},
		{"fast-nonlive-extra", constants.ProfileFastSandbox, "", "8443", false},
	} {
		t.Run(item.name, func(t *testing.T) {
			t.Setenv(constants.EnvExperimentalRevisionRuntime, item.live)
			t.Setenv(constants.EnvMitmproxyExtraPorts, item.extras)
			if item.name == "live-unset" {
				require.NoError(t, os.Unsetenv(constants.EnvMitmproxyExtraPorts))
			}
			err := validateRevisionRuntimeStartup(item.profile)
			if item.wantError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				if item.extras == "8443" {
					// The legacy/all interception list still includes extra ports.
					ports, err := constants.BuildMitmproxyPortList(item.extras)
					require.NoError(t, err)
					require.Equal(t, "80,443,8443", ports)
				}
			}
		})
	}
}

func TestRevisionRuntimeStartupRejectsBeforeProfileOrNetwork(t *testing.T) {
	const child = "OPENSANDBOX_TEST_REVISION_STARTUP_CHILD"
	if os.Getenv(child) == "1" {
		main()
		return
	}
	for _, item := range []struct{ name, profile, extras, diagnostic string }{
		{"sidecar-extra", "", "8443", constants.EnvMitmproxyExtraPorts},
		{"fast-live", constants.ProfileFastSandbox, "", constants.ProfileFastSandbox},
	} {
		t.Run(item.name, func(t *testing.T) {
			t.Setenv(child, "1")
			t.Setenv(constants.EnvExperimentalRevisionRuntime, "true")
			t.Setenv(constants.EnvEgressProfile, item.profile)
			t.Setenv(constants.EnvMitmproxyExtraPorts, item.extras)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRevisionRuntimeStartupRejectsBeforeProfileOrNetwork$")
			output, err := cmd.CombinedOutput()
			require.Error(t, err)
			require.NoError(t, ctx.Err())
			require.Contains(t, string(output), "invalid live revision runtime configuration")
			require.Contains(t, string(output), item.diagnostic)
		})
	}
}
