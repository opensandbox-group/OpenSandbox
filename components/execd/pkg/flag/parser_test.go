// Copyright 2025 Alibaba Group Holding Ltd.
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

package flag

import (
	"flag"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestInitFlagsSanitizesNonPositiveJupyterIdlePollIntervalFromCLI(t *testing.T) {
	previousArgs := os.Args
	previousCommandLine := flag.CommandLine
	defaultPollInterval := 100 * time.Millisecond

	flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ContinueOnError)
	os.Args = []string{previousArgs[0], "--jupyter-idle-poll-interval=0"}
	t.Cleanup(func() {
		os.Args = previousArgs
		flag.CommandLine = previousCommandLine
	})

	InitFlags()

	require.Equal(t, defaultPollInterval, JupyterIdlePollInterval)
}

func TestInitFlagsServerAccessTokenFromEnv(t *testing.T) {
	previousArgs := os.Args
	previousCommandLine := flag.CommandLine
	flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ContinueOnError)
	os.Args = []string{previousArgs[0]}
	t.Cleanup(func() {
		os.Args = previousArgs
		flag.CommandLine = previousCommandLine
	})
	t.Setenv("EXECD_ACCESS_TOKEN", "test-token-from-env")

	InitFlags()

	require.Equal(t, "test-token-from-env", ServerAccessToken)
}

func TestInitFlagsCliOverridesEnvAccessToken(t *testing.T) {
	previousArgs := os.Args
	previousCommandLine := flag.CommandLine
	flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ContinueOnError)
	os.Args = []string{previousArgs[0], "--access-token=cli-token"}
	t.Cleanup(func() {
		os.Args = previousArgs
		flag.CommandLine = previousCommandLine
	})
	t.Setenv("EXECD_ACCESS_TOKEN", "env-token")

	InitFlags()

	require.Equal(t, "cli-token", ServerAccessToken)
}

func TestCommandRecoveryDefaults(t *testing.T) {
	withCommandRecoveryFlagState(t, nil, nil, func() {
		InitFlags()

		require.Equal(t, time.Hour, CommandRecoveryTTL)
		require.Equal(t, 1000, CommandRecoveryMaxTerminal)
	})
}

func TestCommandRecoveryEnvDefaultsAndCLIOverrides(t *testing.T) {
	withCommandRecoveryFlagState(t, map[string]string{
		"EXECD_COMMAND_RECOVERY_TTL":          "30m",
		"EXECD_COMMAND_RECOVERY_MAX_TERMINAL": "42",
	}, nil, func() {
		InitFlags()

		require.Equal(t, 30*time.Minute, CommandRecoveryTTL)
		require.Equal(t, 42, CommandRecoveryMaxTerminal)
	})

	withCommandRecoveryFlagState(t, map[string]string{
		"EXECD_COMMAND_RECOVERY_TTL":          "30m",
		"EXECD_COMMAND_RECOVERY_MAX_TERMINAL": "42",
	}, []string{"--command-recovery-ttl=0", "--command-recovery-max-terminal=0"}, func() {
		InitFlags()

		require.Zero(t, CommandRecoveryTTL)
		require.Zero(t, CommandRecoveryMaxTerminal)
	})
}

func TestCommandRecoveryAcceptsZeroEnvironmentAndMaximumCap(t *testing.T) {
	withCommandRecoveryFlagState(t, map[string]string{
		"EXECD_COMMAND_RECOVERY_TTL":          "0",
		"EXECD_COMMAND_RECOVERY_MAX_TERMINAL": "10000",
	}, nil, func() {
		InitFlags()

		require.Zero(t, CommandRecoveryTTL)
		require.Equal(t, 10000, CommandRecoveryMaxTerminal)
	})
}

func TestCommandRecoveryRejectsInvalidEnvironment(t *testing.T) {
	for _, test := range []struct {
		name string
		env  map[string]string
	}{
		{name: "empty TTL", env: map[string]string{"EXECD_COMMAND_RECOVERY_TTL": ""}},
		{name: "malformed TTL", env: map[string]string{"EXECD_COMMAND_RECOVERY_TTL": "tomorrow"}},
		{name: "negative TTL", env: map[string]string{"EXECD_COMMAND_RECOVERY_TTL": "-1s"}},
		{name: "empty cap", env: map[string]string{"EXECD_COMMAND_RECOVERY_MAX_TERMINAL": ""}},
		{name: "malformed cap", env: map[string]string{"EXECD_COMMAND_RECOVERY_MAX_TERMINAL": "many"}},
		{name: "negative cap", env: map[string]string{"EXECD_COMMAND_RECOVERY_MAX_TERMINAL": "-1"}},
		{name: "oversized cap", env: map[string]string{"EXECD_COMMAND_RECOVERY_MAX_TERMINAL": "10001"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			withCommandRecoveryFlagState(t, test.env, nil, func() {
				require.Panics(t, InitFlags)
			})
		})
	}
}

func TestCommandRecoveryRejectsInvalidCLIValues(t *testing.T) {
	for _, args := range [][]string{
		{"--command-recovery-ttl="},
		{"--command-recovery-ttl=tomorrow"},
		{"--command-recovery-ttl=-1s"},
		{"--command-recovery-max-terminal="},
		{"--command-recovery-max-terminal=many"},
		{"--command-recovery-max-terminal=-1"},
		{"--command-recovery-max-terminal=10001"},
	} {
		t.Run(args[0], func(t *testing.T) {
			withCommandRecoveryFlagState(t, nil, args, func() {
				require.Panics(t, InitFlags)
			})
		})
	}
}

func withCommandRecoveryFlagState(t *testing.T, env map[string]string, args []string, test func()) {
	t.Helper()
	previousArgs := os.Args
	previousCommandLine := flag.CommandLine
	flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ContinueOnError)
	os.Args = []string{previousArgs[0]}
	os.Args = append(os.Args, args...)
	t.Cleanup(func() {
		os.Args = previousArgs
		flag.CommandLine = previousCommandLine
	})

	for _, name := range []string{"EXECD_COMMAND_RECOVERY_TTL", "EXECD_COMMAND_RECOVERY_MAX_TERMINAL"} {
		previous, present := os.LookupEnv(name)
		if value, ok := env[name]; ok {
			require.NoError(t, os.Setenv(name, value))
		} else {
			require.NoError(t, os.Unsetenv(name))
		}
		t.Cleanup(func() {
			if present {
				require.NoError(t, os.Setenv(name, previous))
				return
			}
			require.NoError(t, os.Unsetenv(name))
		})
	}

	test()
}
