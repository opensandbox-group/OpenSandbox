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
	"fmt"
	stdlog "log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/alibaba/opensandbox/execd/pkg/log"
)

const (
	jupyterHostEnv             = "JUPYTER_HOST"
	jupyterTokenEnv            = "JUPYTER_TOKEN"
	accessTokenEnv             = "EXECD_ACCESS_TOKEN"
	gracefulShutdownTimeoutEnv = "EXECD_API_GRACE_SHUTDOWN"
	jupyterIdlePollIntervalEnv = "EXECD_JUPYTER_IDLE_POLL_INTERVAL"
	isolationConfigEnv         = "EXECD_ISOLATION_CONFIG"
	commandRecoveryTTLEnv      = "EXECD_COMMAND_RECOVERY_TTL"
	commandRecoveryMaxEnv      = "EXECD_COMMAND_RECOVERY_MAX_TERMINAL"
	defaultCommandRecoveryTTL  = time.Hour
	defaultCommandRecoveryMax  = 1000
	maxCommandRecoveryTerminal = 10000
)

// InitFlags registers CLI flags and env overrides.
func InitFlags() {
	// Set default values
	ServerPort = 44772
	ServerLogLevel = 6
	ServerAccessToken = ""
	ApiGracefulShutdownTimeout = time.Second * 1
	JupyterIdlePollInterval = 100 * time.Millisecond
	IsolationConfigPath = ""
	CommandRecoveryTTL = defaultCommandRecoveryTTL
	CommandRecoveryMaxTerminal = defaultCommandRecoveryMax

	// First, set default values from environment variables
	if jupyterFromEnv := os.Getenv(jupyterHostEnv); jupyterFromEnv != "" {
		if !strings.HasPrefix(jupyterFromEnv, "http://") && !strings.HasPrefix(jupyterFromEnv, "https://") {
			stdlog.Panic("Invalid JUPYTER_HOST format: must start with http:// or https://")
		}
		JupyterServerHost = jupyterFromEnv
	}

	if jupyterTokenFromEnv := os.Getenv(jupyterTokenEnv); jupyterTokenFromEnv != "" {
		JupyterServerToken = jupyterTokenFromEnv
	}

	if accessTokenFromEnv := os.Getenv(accessTokenEnv); accessTokenFromEnv != "" {
		ServerAccessToken = accessTokenFromEnv
	}

	// Then define flags with current values as defaults
	flag.StringVar(&JupyterServerHost, "jupyter-host", JupyterServerHost, "Jupyter server host address (e.g., http://localhost, http://192.168.1.100)")
	flag.StringVar(&JupyterServerToken, "jupyter-token", JupyterServerToken, "Jupyter server authentication token")
	flag.IntVar(&ServerPort, "port", ServerPort, "Server listening port (default: 44772)")
	flag.IntVar(&ServerLogLevel, "log-level", ServerLogLevel, "Server log level (0=LevelEmergency, 1=LevelAlert, 2=LevelCritical, 3=LevelError, 4=LevelWarning, 5=LevelNotice, 6=LevelInformational, 7=LevelDebug, default: 6)")
	flag.StringVar(&ServerAccessToken, "access-token", ServerAccessToken, "Server access token for API authentication")

	if graceShutdownTimeout := os.Getenv(gracefulShutdownTimeoutEnv); graceShutdownTimeout != "" {
		duration, err := time.ParseDuration(graceShutdownTimeout)
		if err != nil {
			stdlog.Panicf("Failed to parse graceful shutdown timeout from env: %v", err)
		}
		ApiGracefulShutdownTimeout = duration
	}

	if idlePollInterval := os.Getenv(jupyterIdlePollIntervalEnv); idlePollInterval != "" {
		duration, err := time.ParseDuration(idlePollInterval)
		if err != nil {
			stdlog.Panicf("Failed to parse jupyter idle poll interval from env: %v", err)
		}
		if duration <= 0 {
			stdlog.Printf("Invalid %s=%s; fallback to default %s", jupyterIdlePollIntervalEnv, idlePollInterval, JupyterIdlePollInterval)
		} else {
			JupyterIdlePollInterval = duration
		}
	}

	CommandRecoveryTTL = commandRecoveryTTLFromEnv()
	CommandRecoveryMaxTerminal = commandRecoveryMaxFromEnv()

	flag.DurationVar(&ApiGracefulShutdownTimeout, "graceful-shutdown-timeout", ApiGracefulShutdownTimeout, "API graceful shutdown timeout duration (default: 1s)")
	flag.DurationVar(&JupyterIdlePollInterval, "jupyter-idle-poll-interval", JupyterIdlePollInterval, "Polling interval after Jupyter idle status before closing stream (default: 100ms)")
	commandRecoveryTTLFlag := commandRecoveryTTLValue{value: &CommandRecoveryTTL}
	commandRecoveryMaxFlag := commandRecoveryMaxValue{value: &CommandRecoveryMaxTerminal}
	flag.Var(&commandRecoveryTTLFlag, "command-recovery-ttl", "Retention duration for completed command summaries (default: 1h)")
	flag.Var(&commandRecoveryMaxFlag, "command-recovery-max-terminal", "Maximum retained completed command summaries (default: 1000)")

	// Isolation config
	if v := os.Getenv(isolationConfigEnv); v != "" {
		IsolationConfigPath = v
	}
	flag.StringVar(&IsolationConfigPath, "isolation-config", IsolationConfigPath, "Path to isolation TOML config file (default: built-in defaults)")

	// Parse flags - these will override environment variables if provided
	flag.Parse()
	if commandRecoveryTTLFlag.err != nil {
		stdlog.Panicf("Invalid --command-recovery-ttl: %v", commandRecoveryTTLFlag.err)
	}
	if commandRecoveryMaxFlag.err != nil {
		stdlog.Panicf("Invalid --command-recovery-max-terminal: %v", commandRecoveryMaxFlag.err)
	}
	if JupyterIdlePollInterval <= 0 {
		stdlog.Printf("Invalid --jupyter-idle-poll-interval=%s; fallback to default %s", JupyterIdlePollInterval, 100*time.Millisecond)
		JupyterIdlePollInterval = 100 * time.Millisecond
	}

	// Log final values
	log.Info("Jupyter server host is: %s", JupyterServerHost)
	log.Info("Jupyter server token is: %s", log.MaskToken(JupyterServerToken))
}

func commandRecoveryTTLFromEnv() time.Duration {
	value, present := os.LookupEnv(commandRecoveryTTLEnv)
	if !present {
		return CommandRecoveryTTL
	}
	if value == "" {
		stdlog.Panicf("Invalid %s: value must not be empty", commandRecoveryTTLEnv)
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration < 0 {
		stdlog.Panicf("Invalid %s=%q: must be a non-negative duration", commandRecoveryTTLEnv, value)
	}
	return duration
}

func commandRecoveryMaxFromEnv() int {
	value, present := os.LookupEnv(commandRecoveryMaxEnv)
	if !present {
		return CommandRecoveryMaxTerminal
	}
	if value == "" {
		stdlog.Panicf("Invalid %s: value must not be empty", commandRecoveryMaxEnv)
	}
	max, err := parseCommandRecoveryMax(value)
	if err != nil {
		stdlog.Panicf("Invalid %s=%q: %v", commandRecoveryMaxEnv, value, err)
	}
	return max
}

type commandRecoveryTTLValue struct {
	value *time.Duration
	err   error
}

func (v *commandRecoveryTTLValue) String() string {
	return v.value.String()
}

func (v *commandRecoveryTTLValue) Set(value string) error {
	duration, err := time.ParseDuration(value)
	if err != nil {
		v.err = err
		return nil
	}
	if duration < 0 {
		v.err = fmt.Errorf("must be a non-negative duration")
		return nil
	}
	*v.value = duration
	return nil
}

type commandRecoveryMaxValue struct {
	value *int
	err   error
}

func (v *commandRecoveryMaxValue) String() string {
	return strconv.Itoa(*v.value)
}

func (v *commandRecoveryMaxValue) Set(value string) error {
	max, err := parseCommandRecoveryMax(value)
	if err != nil {
		v.err = err
		return nil
	}
	*v.value = max
	return nil
}

func parseCommandRecoveryMax(value string) (int, error) {
	max, err := strconv.Atoi(value)
	if err != nil {
		return 0, err
	}
	if max < 0 || max > maxCommandRecoveryTerminal {
		return 0, fmt.Errorf("must be between 0 and %d", maxCommandRecoveryTerminal)
	}
	return max, nil
}
