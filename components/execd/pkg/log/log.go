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

package log

import (
	"context"
	"os"

	slogger "github.com/alibaba/opensandbox/internal/logger"
	"github.com/alibaba/opensandbox/internal/safego"
)

const logFileEnvKey = "EXECD_LOG_FILE"

var current slogger.Logger

// Init constructs the singleton logger. Call once during startup.
// Legacy levels: 0/1/2=fatal, 3=error, 4=warn, 5/6=info, 7+=debug.
func Init(level int) {
	current = newLogger(mapLevel(level))
	safego.InitPanicLogger(context.Background(), current)
}

func mapLevel(level int) string {
	switch {
	case level <= 2:
		return "fatal"
	case level == 3:
		return "error"
	case level == 4:
		return "warn"
	case level == 5 || level == 6:
		return "info"
	default:
		return "debug"
	}
}

func newLogger(level string) slogger.Logger {
	cfg := slogger.Config{
		Level: level,
	}
	if logFile := os.Getenv(logFileEnvKey); logFile != "" {
		cfg.OutputPaths = []string{logFile}
		cfg.ErrorOutputPaths = cfg.OutputPaths
	}
	return slogger.MustNew(cfg)
}

func getLogger() slogger.Logger {
	if current != nil {
		return current
	}
	l := newLogger("info")
	current = l
	return l
}

func Debug(format string, args ...any) {
	getLogger().Debugf(format, args...)
}

func Info(format string, args ...any) {
	getLogger().Infof(format, args...)
}

func Warn(format string, args ...any) {
	getLogger().Warnf(format, args...)
}

// Warning is an alias to Warn for compatibility.
func Warning(format string, args ...any) {
	Warn(format, args...)
}

func Error(format string, args ...any) {
	getLogger().Errorf(format, args...)
}
