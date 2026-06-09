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
	stdlog "log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/alibaba/opensandbox/execd/pkg/log"
)

const (
	jupyterHostEnv              = "JUPYTER_HOST"
	jupyterTokenEnv             = "JUPYTER_TOKEN"
	accessTokenEnv              = "EXECD_ACCESS_TOKEN"
	gracefulShutdownTimeoutEnv  = "EXECD_API_GRACE_SHUTDOWN"
	jupyterIdlePollIntervalEnv  = "EXECD_JUPYTER_IDLE_POLL_INTERVAL"
	isolationUpperRootEnv       = "EXECD_ISOLATION_UPPER_ROOT"
	isolationUpperMaxBytesEnv   = "EXECD_ISOLATION_UPPER_MAX_BYTES"
	isolationDiffMaxBytesEnv    = "EXECD_ISOLATION_DIFF_MAX_BYTES"
	isolationAllowedWritableEnv = "EXECD_ISOLATION_ALLOWED_WRITABLE"
)

// InitFlags registers CLI flags and env overrides.
func InitFlags() {
	// Set default values
	ServerPort = 44772
	ServerLogLevel = 6
	ServerAccessToken = ""
	ApiGracefulShutdownTimeout = time.Second * 1
	JupyterIdlePollInterval = 100 * time.Millisecond
	IsolationUpperRoot = "/var/lib/execd/isolation"
	IsolationUpperMaxBytes = 8 * 1024 * 1024 * 1024 // 8 GiB
	IsolationDiffMaxBytes = 4 * 1024 * 1024 * 1024  // 4 GiB
	IsolationAllowedWritable = ""                   // reject all

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

	flag.DurationVar(&ApiGracefulShutdownTimeout, "graceful-shutdown-timeout", ApiGracefulShutdownTimeout, "API graceful shutdown timeout duration (default: 1s)")
	flag.DurationVar(&JupyterIdlePollInterval, "jupyter-idle-poll-interval", JupyterIdlePollInterval, "Polling interval after Jupyter idle status before closing stream (default: 100ms)")

	// Isolation flags
	if v := os.Getenv(isolationUpperRootEnv); v != "" {
		IsolationUpperRoot = v
	}
	if v := os.Getenv(isolationUpperMaxBytesEnv); v != "" {
		if n, err := parseInt64(v); err == nil {
			IsolationUpperMaxBytes = n
		}
	}
	if v := os.Getenv(isolationDiffMaxBytesEnv); v != "" {
		if n, err := parseInt64(v); err == nil {
			IsolationDiffMaxBytes = n
		}
	}
	if v := os.Getenv(isolationAllowedWritableEnv); v != "" {
		IsolationAllowedWritable = v
	}
	flag.StringVar(&IsolationUpperRoot, "isolation-upper-root", IsolationUpperRoot, "Parent directory for per-session overlay upper directories")
	flag.Int64Var(&IsolationUpperMaxBytes, "isolation-upper-max-bytes", IsolationUpperMaxBytes, "Hard limit on total upper directory size (default: 8 GiB)")
	flag.Int64Var(&IsolationDiffMaxBytes, "isolation-diff-max-bytes", IsolationDiffMaxBytes, "Max tar.gz diff output size (default: 4 GiB)")
	flag.StringVar(&IsolationAllowedWritable, "isolation-allowed-writable", IsolationAllowedWritable, "Comma-separated allowlist of extra_writable paths")

	// Parse flags - these will override environment variables if provided
	flag.Parse()
	if JupyterIdlePollInterval <= 0 {
		stdlog.Printf("Invalid --jupyter-idle-poll-interval=%s; fallback to default %s", JupyterIdlePollInterval, 100*time.Millisecond)
		JupyterIdlePollInterval = 100 * time.Millisecond
	}

	// Log final values
	log.Info("Jupyter server host is: %s", JupyterServerHost)
	log.Info("Jupyter server token is: %s", log.MaskToken(JupyterServerToken))
}

// parseInt64 parses a decimal int64 from s.
func parseInt64(s string) (int64, error) {
	return strconv.ParseInt(strings.TrimSpace(s), 10, 64)
}
