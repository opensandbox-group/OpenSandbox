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

package mitmproxy

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBuildMitmdumpArgsModeSpecs(t *testing.T) {
	v6 := true
	args := buildMitmdumpArgs(Config{ListenPort: 18081, ListenV6: &v6})
	require.Contains(t, args, "transparent@127.0.0.1:18081")
	require.Contains(t, args, "transparent@::1:18081")
	v6 = false
	args = buildMitmdumpArgs(Config{ListenPort: 18081, ListenV6: &v6})
	require.Contains(t, args, "transparent@127.0.0.1:18081")
	require.NotContains(t, args, "transparent@::1:18081")
}

func TestBuildMitmdumpArgsNoUserScripts(t *testing.T) {
	args := buildMitmdumpArgs(Config{ListenPort: 18081})
	require.Contains(t, args, "--mode")
	require.Contains(t, args, "transparent@127.0.0.1:18081")
	require.Contains(t, args, "--listen-port")
	require.Contains(t, args, "18081")
	require.Contains(t, args, "--set")
	require.Contains(t, args, "flow_detail=0")
	require.Contains(t, args, "-s")
	require.Contains(t, args, systemScriptPath)
	require.NotContains(t, args, "--listen-host", "sidecar: the baked-in config.yaml loopback bind stays")
	// Only one -s (system addon)
	count := 0
	for _, a := range args {
		if a == "-s" {
			count++
		}
	}
	require.Equal(t, 1, count)
}

func TestBuildMitmdumpArgsFastSandboxListenHost(t *testing.T) {
	args := buildMitmdumpArgs(Config{ListenPort: 18081, ListenHost: "0.0.0.0"})
	require.Contains(t, args, "--listen-host")
	require.Contains(t, args, "0.0.0.0")
}

func TestBuildMitmdumpArgsSingleUserScript(t *testing.T) {
	args := buildMitmdumpArgs(Config{
		ListenPort:  18081,
		ScriptPaths: []string{"/scripts/auth.py"},
	})
	count := 0
	for _, a := range args {
		if a == "-s" {
			count++
		}
	}
	require.Equal(t, 2, count)
	require.Equal(t, "/scripts/auth.py", args[len(args)-1])
}

func TestBuildMitmdumpArgsMultipleUserScripts(t *testing.T) {
	args := buildMitmdumpArgs(Config{
		ListenPort:  18081,
		ScriptPaths: []string{"/scripts/auth.py", "/scripts/logging.py"},
	})
	count := 0
	for _, a := range args {
		if a == "-s" {
			count++
		}
	}
	require.Equal(t, 3, count)
	// Order: system, auth, logging
	scripts := []string{}
	for i, a := range args {
		if a == "-s" {
			scripts = append(scripts, args[i+1])
		}
	}
	require.Equal(t, []string{systemScriptPath, "/scripts/auth.py", "/scripts/logging.py"}, scripts)
}

func TestBuildMitmdumpArgsSkipsEmptyScriptPaths(t *testing.T) {
	args := buildMitmdumpArgs(Config{
		ListenPort:  18081,
		ScriptPaths: []string{"  ", "/scripts/auth.py", "", "  /scripts/logging.py  "},
	})
	scripts := []string{}
	for i, a := range args {
		if a == "-s" {
			scripts = append(scripts, args[i+1])
		}
	}
	require.Equal(t, []string{systemScriptPath, "/scripts/auth.py", "/scripts/logging.py"}, scripts)
}

func TestBuildMitmdumpEnvSetsMitmproxyHome(t *testing.T) {
	env := buildMitmdumpEnv(
		[]string{
			"PATH=/usr/bin",
		},
		"/home/mitmproxy",
		nil,
	)

	require.Contains(t, env, "PATH=/usr/bin")
	require.Contains(t, env, "HOME=/home/mitmproxy")
}

func TestBuildMitmdumpEnvHandsOffRevisionIPC(t *testing.T) {
	cfg := &RevisionIPCConfig{
		SocketPath:        "/run/opensandbox/revision/receiver.sock",
		SessionToken:      "0123456789abcdef0123456789abcdef",
		ControlGeneration: "control-a",
		SubjectGeneration: "subject-a",
		MaxSnapshotBytes:  4096,
	}
	require.NoError(t, validateRevisionIPCConfig(cfg))
	env := buildMitmdumpEnv(
		[]string{
			"PATH=/usr/bin",
			revisionIPCTokenEnv + "=stale-secret",
			revisionIPCSocketEnv + "=/tmp/stale.sock",
		},
		"/home/mitmproxy",
		cfg,
	)
	require.Contains(t, env, revisionIPCSocketEnv+"="+cfg.SocketPath)
	require.Contains(t, env, revisionIPCTokenEnv+"="+cfg.SessionToken)
	require.Contains(t, env, revisionIPCControlGenerationEnv+"="+cfg.ControlGeneration)
	require.Contains(t, env, revisionIPCSubjectGenerationEnv+"="+cfg.SubjectGeneration)
	require.Contains(t, env, revisionIPCMaxSnapshotBytesEnv+"=4096")
	require.NotContains(t, env, revisionIPCTokenEnv+"=stale-secret")
	require.NotContains(t, env, revisionIPCSocketEnv+"=/tmp/stale.sock")
}

func TestBuildMitmdumpEnvScrubsDisabledRevisionIPC(t *testing.T) {
	env := buildMitmdumpEnv(
		[]string{
			"PATH=/usr/bin",
			revisionIPCTokenEnv + "=stale-secret",
			revisionIPCSocketEnv + "=/tmp/stale.sock",
		},
		"/home/mitmproxy",
		nil,
	)
	for _, name := range revisionIPCEnvNames {
		for _, value := range env {
			require.NotRegexp(t, "^"+name+"=", value)
		}
	}
}

func TestRevisionIPCConfigRejectsInvalidValuesWithoutSecrets(t *testing.T) {
	valid := RevisionIPCConfig{
		SocketPath:        "/run/opensandbox/revision/receiver.sock",
		SessionToken:      "0123456789abcdef0123456789abcdef",
		ControlGeneration: "control-a",
		SubjectGeneration: "subject-a",
		MaxSnapshotBytes:  4096,
	}
	cases := []RevisionIPCConfig{
		{},
		func() RevisionIPCConfig { candidate := valid; candidate.SocketPath = "relative.sock"; return candidate }(),
		func() RevisionIPCConfig {
			candidate := valid
			candidate.SocketPath = "/run/opensandbox/\x00.sock"
			return candidate
		}(),
		func() RevisionIPCConfig { candidate := valid; candidate.SessionToken = "secret"; return candidate }(),
		func() RevisionIPCConfig { candidate := valid; candidate.ControlGeneration = ""; return candidate }(),
		func() RevisionIPCConfig {
			candidate := valid
			candidate.ControlGeneration = "control\x00a"
			return candidate
		}(),
		func() RevisionIPCConfig { candidate := valid; candidate.SubjectGeneration = ""; return candidate }(),
		func() RevisionIPCConfig { candidate := valid; candidate.MaxSnapshotBytes = 0; return candidate }(),
	}
	for _, candidate := range cases {
		err := validateRevisionIPCConfig(&candidate)
		require.Error(t, err)
		require.Equal(t, "mitmproxy: invalid revision IPC configuration", err.Error())
		if candidate.SessionToken != "" {
			require.NotContains(t, err.Error(), candidate.SessionToken)
		}
	}
}

func TestCredentialProxyMessageStripsMitmTimestamp(t *testing.T) {
	msg, ok := credentialProxyMessage("[12:34:56.789] credential proxy: rejected request after path substitution: /etc")
	require.True(t, ok)
	require.Equal(t, "credential proxy: rejected request after path substitution: /etc", msg)
}

func TestCredentialProxyMessageWithoutTimestamp(t *testing.T) {
	msg, ok := credentialProxyMessage("credential proxy: applied binding=prod")
	require.True(t, ok)
	require.Equal(t, "credential proxy: applied binding=prod", msg)
}

func TestCredentialProxyMessageAcceptsSanitizedFatalRevisionError(t *testing.T) {
	for _, line := range []string{
		"mitmdump: credential proxy: invalid revision runtime configuration",
		"/usr/local/bin/mitmdump: credential proxy: invalid revision runtime configuration",
	} {
		msg, ok := credentialProxyMessage(line)
		require.True(t, ok)
		require.Equal(t, "credential proxy: invalid revision runtime configuration", msg)
	}
	_, ok := credentialProxyMessage("mitmdump: credential proxy: untrusted detail")
	require.False(t, ok)
}

func TestCredentialProxyMessageRejectsNonProxyLines(t *testing.T) {
	_, ok := credentialProxyMessage("172.17.0.1:50210: GET https://example.com/credential proxy: x")
	require.False(t, ok)
	_, ok = credentialProxyMessage("[12:34:56.789] 10.0.0.2:50322: GET https://example.com/")
	require.False(t, ok)
	_, ok = credentialProxyMessage("")
	require.False(t, ok)
}
