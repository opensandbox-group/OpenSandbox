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

func TestBuildMitmdumpArgsNoUserScripts(t *testing.T) {
	args := buildMitmdumpArgs(Config{ListenPort: 18081})
	require.Contains(t, args, "--listen-port")
	require.Contains(t, args, "18081")
	require.Contains(t, args, "--set")
	require.Contains(t, args, "flow_detail=0")
	require.Contains(t, args, "-s")
	require.Contains(t, args, systemScriptPath)
	// Only one -s (system addon)
	count := 0
	for _, a := range args {
		if a == "-s" {
			count++
		}
	}
	require.Equal(t, 1, count)
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
	)

	require.Contains(t, env, "PATH=/usr/bin")
	require.Contains(t, env, "HOME=/home/mitmproxy")
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

func TestCredentialProxyMessageRejectsNonProxyLines(t *testing.T) {
	_, ok := credentialProxyMessage("172.17.0.1:50210: GET https://example.com/credential proxy: x")
	require.False(t, ok)
	_, ok = credentialProxyMessage("[12:34:56.789] 10.0.0.2:50322: GET https://example.com/")
	require.False(t, ok)
	_, ok = credentialProxyMessage("")
	require.False(t, ok)
}
