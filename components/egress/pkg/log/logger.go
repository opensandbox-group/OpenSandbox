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

package log

import (
	"context"
	"os"

	slogger "github.com/alibaba/opensandbox/internal/logger"
)

// Logger is the package-global sink used by egress; WithLogger is called from main after OTel/fields setup.
var Logger slogger.Logger = slogger.MustNew(slogger.Config{Level: "info"}).Named("opensandbox.egress")

// WithLogger installs the process-wide logger for the egress binary.
func WithLogger(ctx context.Context, logger slogger.Logger) context.Context {
	if logger != nil {
		Logger = logger
	}
	return ctx
}

func Debugf(template string, args ...any) {
	Logger.Debugf(template, args...)
}

func Infof(template string, args ...any) {
	Logger.Infof(template, args...)
}

func Warnf(template string, args ...any) {
	Logger.Warnf(template, args...)
}

func Errorf(template string, args ...any) {
	Logger.Errorf(template, args...)
}

func Fatalf(template string, args ...any) {
	Logger.Errorf(template, args...)
	_ = Logger.Sync()
	os.Exit(1)
}
