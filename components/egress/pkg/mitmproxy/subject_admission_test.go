// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

package mitmproxy

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/stretchr/testify/require"
)

// The subject child reads the static passthrough policy only from its handoff:
// main.go rejects an operator-set value for fast-sandbox, and the child env
// builder strips inheritance. parse_handoff treats a missing value as an error,
// so admission must ship the policy explicitly or the child fatals at load().
func TestSubjectAdmissionStaticPassthroughHandoff(t *testing.T) {
	cfg := subjectLaunchHandoff()
	require.NoError(t, json.Unmarshal([]byte(`{"TLSAdmission":true,"TLSCapacity":32,"ProcessTLSCapacity":131072}`), &cfg))
	child, err := WithSubjectRevision(Config{}, cfg)
	require.NoError(t, err)
	env := buildMitmdumpChildEnv([]string{constants.EnvMitmproxyPassthroughHosts + `=["stale.example"]`}, "/home", child)
	require.Contains(t, env, constants.EnvMitmproxyPassthroughHosts+"=[]")
	require.NotContains(t, env, constants.EnvMitmproxyPassthroughHosts+`=["stale.example"]`)

	disabled, err := WithSubjectRevision(Config{}, subjectLaunchHandoff())
	require.NoError(t, err)
	off := buildMitmdumpChildEnv(nil, "/home", disabled)
	for _, value := range off {
		require.False(t, strings.HasPrefix(value, constants.EnvMitmproxyPassthroughHosts+"="),
			"the disabled path must not introduce a static passthrough reader")
	}
}

func TestSubjectAdmissionHandoff(t *testing.T) {
	cfg := subjectLaunchHandoff()
	// JSON allows these behavior tests to run against the unmodified baseline.
	require.NoError(t, json.Unmarshal([]byte(`{"TLSAdmission":true,"TLSCapacity":32,"ProcessTLSCapacity":131072}`), &cfg))
	child, err := WithSubjectRevision(Config{}, cfg)
	require.NoError(t, err)
	env := buildMitmdumpChildEnv(nil, "/home", child)
	for _, field := range []string{"TLS_ADMISSION=true", "TLS_CAPACITY=32", "PROCESS_TLS_CAPACITY=131072"} {
		require.Contains(t, env, subjectRevisionEnvPrefix+field)
	}
	for _, data := range []string{`{"TLSCapacity":0}`, `{"TLSCapacity":1048577}`, `{"ProcessTLSCapacity":0}`} {
		bad := cfg
		require.NoError(t, json.Unmarshal([]byte(data), &bad))
		_, err = WithSubjectRevision(Config{}, bad)
		require.Error(t, err)
	}
}

func TestSubjectAdmissionBudgetEventRedaction(t *testing.T) {
	for _, suffix := range []string{"secret-subject 1 32", "subject -1 32", "process 33 32", "process 1 1048577", "process 1 0", "subject 1 32 secret", "subject\t1 32"} {
		_, ok := credentialProxyMessage("credential proxy: tls-budget-v1 " + suffix)
		require.False(t, ok, "invalid budget framing must not reach logs or metric labels")
	}
	_, ok := credentialProxyMessage("credential proxy: tls-budget-v1 process 0 131072")
	require.True(t, ok)
}
