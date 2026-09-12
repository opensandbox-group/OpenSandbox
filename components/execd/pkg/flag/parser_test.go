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

func TestOperationCapacityFlags(t *testing.T) {
	for _, tc := range []struct {
		name    string
		env     string
		args    []string
		want    int
		invalid bool
	}{
		{name: "default", want: 4096},
		{name: "environment", env: "12000", want: 12000},
		{name: "cli overrides environment", env: "12000", args: []string{"--operation-capacity=24000"}, want: 24000},
		{name: "zero environment", env: "0", invalid: true},
		{name: "invalid environment", env: "not-a-number", invalid: true},
		{name: "negative cli", args: []string{"--operation-capacity=-1"}, invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			previousArgs, previousFlags, previousCapacity := os.Args, flag.CommandLine, OperationCapacity
			t.Cleanup(func() { os.Args, flag.CommandLine, OperationCapacity = previousArgs, previousFlags, previousCapacity })
			flag.CommandLine = flag.NewFlagSet("execd-capacity-test", flag.ContinueOnError)
			os.Args = append([]string{"execd-capacity-test"}, tc.args...)
			t.Setenv(operationCapacityEnv, tc.env)
			if tc.invalid {
				require.Panics(t, InitFlags)
				return
			}
			InitFlags()
			require.Equal(t, tc.want, OperationCapacity)
		})
	}
}
