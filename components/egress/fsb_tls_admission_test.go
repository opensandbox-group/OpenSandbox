// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
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
	const flag = "OPENSANDBOX_EGRESS_EXPERIMENTAL_FSB_TLS_ADMISSION"
	t.Setenv(flag, "true")
	t.Setenv(constants.EnvExperimentalRevisionRuntime, "false")
	err := validateRevisionRuntimeStartup(constants.ProfileFastSandbox)
	require.ErrorContains(t, err, constants.EnvExperimentalRevisionRuntime)
	t.Setenv(constants.EnvExperimentalRevisionRuntime, "true")
	t.Setenv(constants.EnvMitmproxyExtraPorts, "8443")
	require.NoError(t, validateRevisionRuntimeStartup(constants.ProfileFastSandbox))
}
