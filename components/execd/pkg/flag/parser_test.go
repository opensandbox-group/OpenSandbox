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
