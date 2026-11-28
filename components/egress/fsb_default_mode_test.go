// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"
	"testing"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/stretchr/testify/require"
)

func TestFSBDefaultStartupEnablesValidatedAdmission(t *testing.T) {
	clearFSBInterceptionEnv(t)
	old := fsbMitmproxyHome
	calls := 0
	fsbMitmproxyHome = func() (string, error) { calls++; return t.TempDir(), nil }
	t.Cleanup(func() { fsbMitmproxyHome = old })
	before := os.Environ()
	mode, err := revisionRuntimeStartup(constants.ProfileFastSandbox)
	require.NoError(t, err)
	require.Equal(t, fastSandboxInterceptionMode{installReceiver: true, credentialBound: true}, mode)
	require.Equal(t, 1, calls, "default must validate the real startup configuration")
	require.Equal(t, before, os.Environ())
	fsbMitmproxyHome = func() (string, error) { return "", fmt.Errorf("private lookup detail") }
	_, err = revisionRuntimeStartup(constants.ProfileFastSandbox)
	require.ErrorContains(t, err, "configuration unavailable")
	require.ErrorContains(t, err, constants.EnvFSBInterceptionMode+"=all")
	require.NotContains(t, err.Error(), "private")
}

func TestFSBDefaultRejectsDisabledLegacyGates(t *testing.T) {
	for _, key := range []string{constants.EnvExperimentalRevisionRuntime, constants.EnvExperimentalFSBTLSAdmission} {
		t.Run(key, func(t *testing.T) {
			clearFSBInterceptionEnv(t)
			t.Setenv(key, "false")
			_, err := revisionRuntimeStartup(constants.ProfileFastSandbox)
			require.ErrorContains(t, err, key)
			require.ErrorContains(t, err, constants.EnvFSBInterceptionMode+"=all")
		})
	}
}

// This same test also runs unchanged against the pre-default-change source.
func TestFSBAllAndSidecarStartupDifferential(t *testing.T) {
	for _, profile := range []string{constants.ProfileFastSandbox, constants.ProfileSidecar, ""} {
		t.Run(profile, func(t *testing.T) {
			clearFSBInterceptionEnv(t)
			if profile == constants.ProfileFastSandbox {
				t.Setenv(constants.EnvFSBInterceptionMode, "all")
			}
			t.Setenv(constants.EnvMitmproxyTransparent, "false")
			t.Setenv(constants.EnvMitmproxyExtraPorts, "8443")
			t.Setenv(constants.EnvMitmproxySslInsecure, "true")
			t.Setenv(constants.EnvUpstreamProxy, "https://fixture.invalid:443")
			oldHome, oldMode := fsbMitmproxyHome, startupFSBInterceptionMode
			fsbMitmproxyHome = func() (string, error) {
				t.Fatal("legacy startup must not read credential-bound configuration")
				return "", nil
			}
			t.Cleanup(func() { fsbMitmproxyHome, startupFSBInterceptionMode = oldHome, oldMode })
			before := os.Environ()
			mode, err := revisionRuntimeStartup(profile)
			require.NoError(t, err)
			require.Equal(t, fastSandboxInterceptionMode{}, mode)
			require.Equal(t, before, os.Environ())
			if profile == constants.ProfileFastSandbox {
				startupFSBInterceptionMode = &mode
				owner, err := newFastSandboxSubjectLaunchOwner(false, -1, -1)
				require.NoError(t, err)
				require.Nil(t, owner)
			}
		})
	}
}
