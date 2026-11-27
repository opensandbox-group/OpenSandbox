// Copyright 2025 The OpenSandbox Authors
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

func TestInitFlagsJupyterConnectionEnv(t *testing.T) {
	for _, tc := range []struct {
		name, host, wantHost, wantToken string
		args                            []string
		wantPanic                       bool
	}{
		{name: "unset", wantHost: "http://default", wantToken: "default-token"},
		{name: "http", host: "http://env", wantHost: "http://env", wantToken: "env-token"},
		{name: "https", host: "https://env", wantHost: "https://env", wantToken: "env-token"},
		{name: "cli overrides env", host: "http://env", args: []string{"--jupyter-host=https://cli", "--jupyter-token=cli-token"}, wantHost: "https://cli", wantToken: "cli-token"},
		{name: "invalid host", host: "invalid", wantPanic: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			previousArgs, previousCommandLine := os.Args, flag.CommandLine
			previousHost, previousToken := JupyterServerHost, JupyterServerToken
			t.Cleanup(func() {
				os.Args, flag.CommandLine = previousArgs, previousCommandLine
				JupyterServerHost, JupyterServerToken = previousHost, previousToken
			})
			flag.CommandLine = flag.NewFlagSet(previousArgs[0], flag.ContinueOnError)
			os.Args = append([]string{previousArgs[0]}, tc.args...)
			JupyterServerHost, JupyterServerToken = "http://default", "default-token"
			t.Setenv(jupyterHostEnv, tc.host)
			t.Setenv(jupyterTokenEnv, "env-token")
			if tc.name == "unset" {
				t.Setenv(jupyterTokenEnv, "")
			}
			if tc.wantPanic {
				require.Panics(t, InitFlags)
				return
			}
			InitFlags()
			require.Equal(t, tc.wantHost, JupyterServerHost)
			require.Equal(t, tc.wantToken, JupyterServerToken)
		})
	}
}

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

func TestInitFlagsDefaultsGracefulShutdownTimeout(t *testing.T) {
	previousArgs := os.Args
	previousCommandLine := flag.CommandLine
	flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ContinueOnError)
	os.Args = []string{previousArgs[0]}
	t.Cleanup(func() {
		os.Args = previousArgs
		flag.CommandLine = previousCommandLine
	})
	t.Setenv(gracefulShutdownTimeoutEnv, "")

	InitFlags()

	require.Equal(t, 200*time.Millisecond, ApiGracefulShutdownTimeout)
}

func TestInitFlagsDefaultsSseLegacyCompat(t *testing.T) {
	previousArgs := os.Args
	previousCommandLine := flag.CommandLine
	flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ContinueOnError)
	os.Args = []string{previousArgs[0]}
	t.Cleanup(func() {
		os.Args = previousArgs
		flag.CommandLine = previousCommandLine
	})
	t.Setenv(sseLegacyCompatEnv, "")

	InitFlags()

	require.False(t, SseLegacyCompat)
}

func TestInitFlagsSseLegacyCompatFromEnv(t *testing.T) {
	previousArgs := os.Args
	previousCommandLine := flag.CommandLine
	flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ContinueOnError)
	os.Args = []string{previousArgs[0]}
	t.Cleanup(func() {
		os.Args = previousArgs
		flag.CommandLine = previousCommandLine
	})
	t.Setenv(sseLegacyCompatEnv, "true")

	InitFlags()

	require.True(t, SseLegacyCompat)
}

func TestInitFlagsInitAuthFromEnvAndCLI(t *testing.T) {
	reset := func(t *testing.T, args ...string) {
		t.Helper()
		previousArgs := os.Args
		previousCommandLine := flag.CommandLine
		flag.CommandLine = flag.NewFlagSet(previousArgs[0], flag.ContinueOnError)
		os.Args = append([]string{previousArgs[0]}, args...)
		t.Cleanup(func() {
			os.Args = previousArgs
			flag.CommandLine = previousCommandLine
		})
	}

	t.Run("defaults false when unset", func(t *testing.T) {
		reset(t)
		t.Setenv(initAuthEnv, "")
		InitFlags()
		require.False(t, InitAuth)
	})

	t.Run("parses true and 0 from env", func(t *testing.T) {
		reset(t)
		t.Setenv(initAuthEnv, "true")
		InitFlags()
		require.True(t, InitAuth)

		reset(t)
		t.Setenv(initAuthEnv, "0")
		InitFlags()
		require.False(t, InitAuth)
	})

	t.Run("cli flag overrides env", func(t *testing.T) {
		reset(t, "--init-auth=false")
		t.Setenv(initAuthEnv, "true")
		InitFlags()
		require.False(t, InitAuth)
	})

	t.Run("invalid env panics", func(t *testing.T) {
		reset(t)
		t.Setenv(initAuthEnv, "notabool")
		require.Panics(t, InitFlags)
	})
}
