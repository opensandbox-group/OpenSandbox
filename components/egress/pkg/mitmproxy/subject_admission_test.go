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

func TestSubjectAdmissionHTTP1OnlyAndDisabledExactArgs(t *testing.T) {
	for _, name := range []string{constants.EnvMitmproxyUpstreamTrustDir, constants.EnvMitmproxyUpstreamExtraCA, constants.EnvMitmproxySslInsecure, constants.EnvUpstreamProxy} {
		t.Setenv(name, "")
	}
	off, err := WithSubjectRevision(Config{ListenPort: 18080}, subjectLaunchHandoff())
	require.NoError(t, err)
	expected := []string{"--mode", "transparent@127.0.0.1:18080", "--listen-port", "18080", "--set", "flow_detail=0", "-s", systemScriptPath, "-s", SubjectRevisionScriptPath}
	require.Equal(t, expected, buildMitmdumpArgs(off))
	expectedEnv := []string{"PATH=/bin", "KEEP=value", "HOME=/home",
		subjectRevisionEnvPrefix + "MODE=subject-v2", subjectRevisionEnvPrefix + "SOCKET=/private/receiver.sock",
		subjectRevisionEnvPrefix + "TOKEN=" + strings.Repeat("a", 32), subjectRevisionEnvPrefix + "CONTROL_GENERATION=control",
		subjectRevisionEnvPrefix + "MAX_SUBJECTS=4096", subjectRevisionEnvPrefix + "MAX_SNAPSHOT_BYTES=8388608",
		subjectRevisionEnvPrefix + "RETAINED_PAYLOAD_BUDGET=68719476736"}
	require.Equal(t, expectedEnv, buildMitmdumpChildEnv([]string{"PATH=/bin", "KEEP=value"}, "/home", off))
	on := *off.SubjectRevisionIPC
	on.TLSAdmission, on.TLSCapacity, on.ProcessTLSCapacity = true, 32, 131072
	enabled := off
	enabled.SubjectRevisionIPC = &on
	require.Contains(t, buildMitmdumpArgs(enabled), "http2=false")
	require.Equal(t, expected, buildMitmdumpArgs(off))
	require.Equal(t, expectedEnv, buildMitmdumpChildEnv([]string{"PATH=/bin", "KEEP=value"}, "/home", off))
	require.NotContains(t, buildMitmdumpArgs(Config{ListenPort: 18080}), "http2=false")
}
