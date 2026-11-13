// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

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

func TestFSBTLSAdmissionConcurrentBudget(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "python3", "-m", "unittest", "discover", "-v", "-s", "tests", "-p", "test_subject_admission.py")
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", output)
}

func TestFSBTLSAdmissionStartup(t *testing.T) {
	clearFSBInterceptionEnv(t)
	const flag = "OPENSANDBOX_EGRESS_EXPERIMENTAL_FSB_TLS_ADMISSION"
	t.Setenv(flag, "true")
	t.Setenv(constants.EnvExperimentalRevisionRuntime, "false")
	err := validateRevisionRuntimeStartup(constants.ProfileFastSandbox)
	require.ErrorContains(t, err, constants.EnvExperimentalRevisionRuntime)
	t.Setenv(constants.EnvExperimentalRevisionRuntime, "true")
	t.Setenv(constants.EnvMitmproxyExtraPorts, "8443")
	require.ErrorContains(t, validateRevisionRuntimeStartup(constants.ProfileFastSandbox), constants.EnvMitmproxyExtraPorts)
}

func TestFSBInterceptionSessionUsesResolvedMode(t *testing.T) {
	for _, tc := range []struct {
		name, mode, revision, admission string
		want                            bool
	}{
		{"new-bound", "credential-bound", "", "", true},
		{"legacy-bound", "", "true", "true", true},
		{"installation-only", "", "true", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearFSBInterceptionEnv(t)
			if tc.mode != "" {
				t.Setenv(constants.EnvFSBInterceptionMode, tc.mode)
			}
			if tc.revision != "" {
				t.Setenv(constants.EnvExperimentalRevisionRuntime, tc.revision)
			}
			if tc.admission != "" {
				t.Setenv(constants.EnvExperimentalFSBTLSAdmission, tc.admission)
			}
			mode, err := resolveFastSandboxInterceptionMode(constants.ProfileFastSandbox)
			require.NoError(t, err)
			require.True(t, mode.installReceiver)
			// Mutating the raw gate after resolution must not alter this session or a
			// replacement child: only the owner's captured launch config is consulted.
			t.Setenv(constants.EnvExperimentalFSBTLSAdmission, "false")
			t.Setenv(constants.EnvFSBTLSCapacity, "17")
			t.Setenv(constants.EnvFSBProcessTLSCapacity, "100")
			parent, err := os.MkdirTemp("", "r4-")
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, os.RemoveAll(parent)) })
			cfg := fastSandboxSubjectSessionConfig{parent: parent, uid: os.Getuid(), gid: os.Getgid(), tlsAdmission: mode.credentialBound}
			for i := 0; i < 2; i++ {
				session, err := newFastSandboxSubjectSession(cfg)
				require.NoError(t, err)
				require.Equal(t, tc.want, session.handoff.TLSAdmission)
				if tc.want {
					require.Equal(t, 17, session.handoff.TLSCapacity)
					require.Equal(t, 100, session.handoff.ProcessTLSCapacity)
				}
				require.NoError(t, session.close())
			}
		})
	}
}
