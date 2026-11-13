// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

package mitmproxy

import (
	"os"
	"testing"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/stretchr/testify/require"
)

// These exact ordered vectors are frozen from 658aa63fe58e. Run this same test
// on that baseline and the candidate; do not normalize, sort or omit entries.
func TestFSBInterceptionDisabledLaunchUnchanged(t *testing.T) {
	for _, name := range []string{
		"OPENSANDBOX_EGRESS_FSB_INTERCEPTION_MODE",
		constants.EnvExperimentalRevisionRuntime, constants.EnvExperimentalFSBTLSAdmission,
		constants.EnvMitmproxyUpstreamTrustDir, constants.EnvMitmproxyUpstreamExtraCA,
		constants.EnvMitmproxySslInsecure, constants.EnvUpstreamProxy,
	} {
		t.Setenv(name, "")
		require.NoError(t, os.Unsetenv(name))
	}
	cfg := Config{ListenPort: 18081, ListenHost: "0.0.0.0", UserName: RunAsUser, ScriptPaths: []string{"/custom.py"}}
	require.Equal(t, []string{
		"--mode", "transparent@0.0.0.0:18081", "--listen-port", "18081",
		"--set", "flow_detail=0", "--listen-host", "0.0.0.0",
		"-s", "/var/egress/mitmscripts/system.py", "-s", "/custom.py",
	}, buildMitmdumpArgs(cfg))
	base := []string{
		"PATH=/fixture/bin", "BASE=unchanged",
		revisionIPCSocketEnv + "=/stale/socket", revisionIPCTokenEnv + "=stale-token",
		subjectRevisionEnvPrefix + "MODE=stale-mode",
	}
	require.Equal(t, []string{"PATH=/fixture/bin", "BASE=unchanged", subjectRevisionEnvPrefix + "MODE=stale-mode", "HOME=/var/lib/mitmproxy"},
		buildMitmdumpChildEnv(base, "/var/lib/mitmproxy", cfg))
	t.Logf("argv=%q", buildMitmdumpArgs(cfg))
	t.Logf("env=%q", buildMitmdumpChildEnv(base, "/var/lib/mitmproxy", cfg))
}
