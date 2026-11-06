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

package mitmproxy

import (
	"math"
	"strings"
	"testing"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/stretchr/testify/require"
)

func TestSubjectLaunchDisabledGolden(t *testing.T) {
	for _, name := range []string{constants.EnvUpstreamProxy, constants.EnvMitmproxySslInsecure, constants.EnvMitmproxyUpstreamTrustDir, constants.EnvMitmproxyUpstreamExtraCA} {
		t.Setenv(name, "")
	}
	v6 := false
	for _, host := range []string{"", "0.0.0.0"} {
		cfg := Config{ListenPort: 18081, ListenHost: host, ListenV6: &v6, ScriptPaths: []string{"/custom.py"}}
		address := "127.0.0.1"
		if host != "" {
			address = host
		}
		want := []string{"--mode", "transparent@" + address + ":18081", "--listen-port", "18081", "--set", "flow_detail=0"}
		if host != "" {
			want = append(want, "--listen-host", host)
		}
		want = append(want, "-s", systemScriptPath, "-s", "/custom.py")
		require.Equal(t, want, buildMitmdumpArgs(cfg))
		require.Equal(t, []string{"/custom.py"}, cfg.ScriptPaths)
		base := []string{"PATH=/bin", revisionIPCSocketEnv + "=stale", revisionIPCTokenEnv + "=stale", subjectRevisionEnvPrefix + "MODE=inherited"}
		// Literal baseline: the new handoff names are not filtered on the old path.
		require.Equal(t, []string{"PATH=/bin", subjectRevisionEnvPrefix + "MODE=inherited", "HOME=/home/mitmproxy"}, buildMitmdumpChildEnv(base, "/home/mitmproxy", cfg))
	}
	// A sidecar v1/live config still delegates to its unchanged environment builder.
	cfg := Config{RevisionIPC: &RevisionIPCConfig{SocketPath: "/p/s", SessionToken: strings.Repeat("a", 32), ControlGeneration: "c", SubjectGeneration: "s", MaxSnapshotBytes: 42}}
	base := []string{"PATH=/bin", revisionIPCTokenEnv + "=stale"}
	require.Equal(t, buildMitmdumpEnv(base, "/home", cfg.RevisionIPC), buildMitmdumpChildEnv(base, "/home", cfg))
}

func subjectLaunchHandoff() SubjectRevisionIPCConfig {
	return SubjectRevisionIPCConfig{SocketPath: "/private/receiver.sock", SessionToken: strings.Repeat("a", 32), ControlGeneration: "control", MaxSubjects: 4096, MaxSnapshotBytes: 8 << 20, RetainedPayloadBudget: 2 * 4096 * int64(8<<20)}
}

func TestSubjectLaunchHandoff(t *testing.T) {
	template := Config{ScriptPaths: []string{"/custom.py"}}
	cfg, err := WithSubjectRevision(template, subjectLaunchHandoff())
	require.NoError(t, err)
	require.Equal(t, []string{"/custom.py"}, template.ScriptPaths)
	require.Equal(t, []string{"/custom.py", SubjectRevisionScriptPath}, cfg.ScriptPaths)
	require.Nil(t, cfg.RevisionIPC)
	env := buildMitmdumpChildEnv([]string{revisionIPCSocketEnv + "=stale", subjectRevisionEnvPrefix + "TOKEN=stale", subjectRevisionEnvPrefix + "UNKNOWN=stale", "PATH=/bin"}, "/home", cfg)
	require.Equal(t, []string{"PATH=/bin", "HOME=/home",
		subjectRevisionEnvPrefix + "MODE=subject-v2", subjectRevisionEnvPrefix + "SOCKET=/private/receiver.sock",
		subjectRevisionEnvPrefix + "TOKEN=" + strings.Repeat("a", 32), subjectRevisionEnvPrefix + "CONTROL_GENERATION=control",
		subjectRevisionEnvPrefix + "MAX_SUBJECTS=4096", subjectRevisionEnvPrefix + "MAX_SNAPSHOT_BYTES=8388608",
		subjectRevisionEnvPrefix + "RETAINED_PAYLOAD_BUDGET=68719476736"}, env)
}

func TestSubjectLaunchInvalidHandoff(t *testing.T) {
	for _, mutate := range []func(*SubjectRevisionIPCConfig){
		func(c *SubjectRevisionIPCConfig) { c.SocketPath = "relative" },
		func(c *SubjectRevisionIPCConfig) { c.SocketPath = "/" + strings.Repeat("x", 103) },
		func(c *SubjectRevisionIPCConfig) { c.SocketPath = "/x\x00y" },
		func(c *SubjectRevisionIPCConfig) { c.SessionToken = "short" },
		func(c *SubjectRevisionIPCConfig) { c.ControlGeneration = "" },
		func(c *SubjectRevisionIPCConfig) { c.ControlGeneration = strings.Repeat("x", 129) },
		func(c *SubjectRevisionIPCConfig) { c.MaxSubjects = 0 },
		func(c *SubjectRevisionIPCConfig) { c.MaxSnapshotBytes = 0 },
		func(c *SubjectRevisionIPCConfig) { c.MaxSnapshotBytes = math.MaxInt },
		func(c *SubjectRevisionIPCConfig) { c.RetainedPayloadBudget-- },
	} {
		cfg := subjectLaunchHandoff()
		mutate(&cfg)
		_, err := WithSubjectRevision(Config{}, cfg)
		require.ErrorIs(t, err, errInvalidRevisionIPCConfig)
	}
	_, err := WithSubjectRevision(Config{RevisionIPC: &RevisionIPCConfig{}}, subjectLaunchHandoff())
	require.Error(t, err)
	cfg, err := WithSubjectRevision(Config{}, subjectLaunchHandoff())
	require.NoError(t, err)
	cfg.ScriptPaths = nil
	require.Error(t, validateSubjectRevisionIPCConfig(cfg))
	cfg.ScriptPaths = []string{SubjectRevisionScriptPath, SubjectRevisionScriptPath}
	require.Error(t, validateSubjectRevisionIPCConfig(cfg))
}
