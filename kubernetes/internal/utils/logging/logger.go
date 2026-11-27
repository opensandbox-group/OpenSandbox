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

package logging

import (
	"os"

	"github.com/go-logr/logr"
	zap2 "go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"gopkg.in/natefinch/lumberjack.v2"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

type syncableLumberjack struct {
	*lumberjack.Logger
}

func (s *syncableLumberjack) Sync() error {
	// Lumberjack exposes no file flush operation; Sync is a no-op.
	return nil
}

type Options struct {
	Development      bool
	EnableFileOutput bool
	LogFilePath      string
	// MaxSize is the rotation threshold in megabytes.
	MaxSize    int
	MaxBackups int
	// MaxAge is the retention limit in days.
	MaxAge int
	// Compress uses gzip for rotated files.
	Compress   bool
	ZapOptions zap.Options
}

// NewLoggerWithZapOptions creates a logger using controller-runtime's zap options
// and adds file output support
func NewLoggerWithZapOptions(opts Options) logr.Logger {
	if opts.ZapOptions.ZapOpts == nil {
		opts.ZapOptions.ZapOpts = []zap2.Option{}
	}
	opts.ZapOptions.ZapOpts = append(opts.ZapOptions.ZapOpts, zap2.AddCaller())

	if !opts.EnableFileOutput {
		return zap.New(zap.UseFlagOptions(&opts.ZapOptions))
	}

	fileWriter := &syncableLumberjack{&lumberjack.Logger{
		Filename:   opts.LogFilePath,
		MaxSize:    opts.MaxSize,
		MaxBackups: opts.MaxBackups,
		MaxAge:     opts.MaxAge,
		Compress:   opts.Compress,
		LocalTime:  true,
	}}

	multiWriter := zapcore.NewMultiWriteSyncer(
		zapcore.AddSync(os.Stdout),
		zapcore.AddSync(fileWriter),
	)

	return zap.New(
		zap.UseFlagOptions(&opts.ZapOptions),
		zap.WriteTo(multiWriter),
	)
}
