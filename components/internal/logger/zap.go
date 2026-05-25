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

package logger

import (
	"fmt"
	"os"
	"strings"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"gopkg.in/natefinch/lumberjack.v2"
)

const envLogOutput = "OPENSANDBOX_LOG_OUTPUT"

const (
	// DefaultRotateMaxSize is the default max size in megabytes before rotation.
	DefaultRotateMaxSize = 100
	// DefaultRotateMaxAge is the default max days to retain old log files.
	DefaultRotateMaxAge = 30
	// DefaultRotateMaxBackups is the default max old files to retain.
	DefaultRotateMaxBackups = 10
)

// RotateConfig controls log file rotation for file-based output paths.
type RotateConfig struct {
	// MaxSize is the maximum size in megabytes before rotation (default 100).
	MaxSize int
	// MaxAge is the maximum number of days to retain old log files (default 30).
	MaxAge int
	// MaxBackups is the maximum number of old log files to retain (default 10).
	MaxBackups int
	// Compress determines whether rotated files are gzip-compressed (default true).
	Compress bool
}

func (r *RotateConfig) applyDefaults() {
	if r.MaxSize <= 0 {
		r.MaxSize = DefaultRotateMaxSize
	}
	if r.MaxAge <= 0 {
		r.MaxAge = DefaultRotateMaxAge
	}
	if r.MaxBackups <= 0 {
		r.MaxBackups = DefaultRotateMaxBackups
	}
}

// Config is the minimal configuration to align execd/ingress defaults.
// - JSON encoding, ISO8601 time
// - Caller/stacktrace disabled
// - Stdout as default output
// - Level defaults to info
type Config struct {
	Level            string        // debug|info|warn|error|fatal (default: info)
	OutputPaths      []string      // default: stdout
	ErrorOutputPaths []string      // default: OutputPaths
	Rotate           *RotateConfig // nil means no rotation on file outputs
}

// New creates a zap-backed Logger with the provided config.
// Log file rotation is enabled by default for file-based output paths.
func New(cfg Config) (Logger, error) {
	cfg = applyEnvOutputs(cfg)
	if cfg.Rotate == nil {
		cfg.Rotate = &RotateConfig{}
	}
	return newWithRotate(cfg)
}

// MustNew is a convenience helper that panics on error.
func MustNew(cfg Config) Logger {
	l, err := New(cfg)
	if err != nil {
		panic(err)
	}
	return l
}

// NewWithExtraCores tees extra zap cores after the production JSON core (e.g. OTLP).
func NewWithExtraCores(cfg Config, extra ...zapcore.Core) (Logger, error) {
	if len(extra) == 0 {
		return New(cfg)
	}
	cfg = applyEnvOutputs(cfg)
	if cfg.Rotate == nil {
		cfg.Rotate = &RotateConfig{}
	}
	return newWithRotate(cfg, extra...)
}

// newWithRotate builds a Logger with lumberjack-backed file writers.
func newWithRotate(cfg Config, extra ...zapcore.Core) (Logger, error) {
	cfg.Rotate.applyDefaults()

	// Fail fast on bad paths/permissions, matching the original zap.Config.Build
	// behaviour that opens sinks at init time.
	for _, paths := range [][]string{cfg.OutputPaths, cfg.ErrorOutputPaths} {
		for _, path := range paths {
			if err := validateOutputPath(path); err != nil {
				return nil, fmt.Errorf("invalid output path %q: %w", path, err)
			}
		}
	}

	encoderCfg := zap.NewProductionEncoderConfig()
	encoderCfg.EncodeTime = zapcore.ISO8601TimeEncoder
	encoderCfg.CallerKey = ""
	encoderCfg.StacktraceKey = ""
	encoder := zapcore.NewJSONEncoder(encoderCfg)

	atom := zap.NewAtomicLevelAt(parseLevel(cfg.Level))

	var cores []zapcore.Core
	for _, path := range cfg.OutputPaths {
		cores = append(cores, zapcore.NewCore(encoder, rotateWriter(path, cfg.Rotate), atom))
	}

	core := teeCores(cores)
	if len(extra) > 0 {
		core = zapcore.NewTee(append([]zapcore.Core{core}, extra...)...)
	}

	// Wire error output paths into zap's internal error sink (encoder/sync
	// failures, etc.) so they respect the same rotation config as regular logs.
	var errSinks []zapcore.WriteSyncer
	for _, path := range cfg.ErrorOutputPaths {
		errSinks = append(errSinks, rotateWriter(path, cfg.Rotate))
	}

	base := zap.New(core,
		zap.ErrorOutput(zapcore.NewMultiWriteSyncer(errSinks...)),
		zap.WrapCore(func(c zapcore.Core) zapcore.Core {
			return zapcore.NewSamplerWithOptions(c, time.Second, 100, 100)
		}),
	)
	return &zapLogger{base: base, sugar: base.Sugar()}, nil
}

// validateOutputPath fails fast on bad paths/permissions by trying to open the
// sink. stdout/stderr are always valid.
func validateOutputPath(path string) error {
	if path == "stdout" || path == "stderr" {
		return nil
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0644)
	if err != nil {
		return err
	}
	return f.Close()
}

// rotateWriter returns a WriteSyncer for the given path using lumberjack
// for file paths, os.Stdout/os.Stderr for console paths.
func rotateWriter(path string, rc *RotateConfig) zapcore.WriteSyncer {
	switch path {
	case "stdout":
		return zapcore.AddSync(os.Stdout)
	case "stderr":
		return zapcore.AddSync(os.Stderr)
	default:
		return zapcore.AddSync(&lumberjack.Logger{
			Filename:   path,
			MaxSize:    rc.MaxSize,
			MaxAge:     rc.MaxAge,
			MaxBackups: rc.MaxBackups,
			Compress:   rc.Compress,
		})
	}
}

func teeCores(cores []zapcore.Core) zapcore.Core {
	switch len(cores) {
	case 0:
		return zapcore.NewCore(
			zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()),
			zapcore.AddSync(os.Stdout),
			zap.NewAtomicLevelAt(zapcore.InfoLevel),
		)
	case 1:
		return cores[0]
	default:
		return zapcore.NewTee(cores...)
	}
}

// AsZapSugared returns the underlying zap SugaredLogger when available.
func AsZapSugared(l Logger) (*zap.SugaredLogger, bool) {
	zl, ok := l.(*zapLogger)
	if !ok {
		return nil, false
	}
	return zl.sugar, true
}

type zapLogger struct {
	base  *zap.Logger
	sugar *zap.SugaredLogger
}

func (l *zapLogger) Debugf(template string, args ...any) {
	l.sugar.Debugf(template, args...)
}

func (l *zapLogger) Infof(template string, args ...any) {
	l.sugar.Infof(template, args...)
}

func (l *zapLogger) Warnf(template string, args ...any) {
	l.sugar.Warnf(template, args...)
}

func (l *zapLogger) Errorf(template string, args ...any) {
	l.sugar.Errorf(template, args...)
}

func (l *zapLogger) With(fields ...Field) Logger {
	if len(fields) == 0 {
		return l
	}
	zfs := make([]zap.Field, 0, len(fields))
	for _, f := range fields {
		zfs = append(zfs, zap.Any(f.Key, f.Value))
	}
	nb := l.base.With(zfs...)
	return &zapLogger{base: nb, sugar: nb.Sugar()}
}

func (l *zapLogger) Named(name string) Logger {
	nb := l.base.Named(name)
	return &zapLogger{base: nb, sugar: nb.Sugar()}
}

func (l *zapLogger) Sync() error {
	return l.base.Sync()
}

func parseLevel(level string) zapcore.Level {
	switch strings.ToLower(level) {
	case "debug":
		return zapcore.DebugLevel
	case "warn", "warning":
		return zapcore.WarnLevel
	case "error":
		return zapcore.ErrorLevel
	case "fatal":
		return zapcore.FatalLevel
	default:
		return zapcore.InfoLevel
	}
}

func applyEnvOutputs(cfg Config) Config {
	envVal := strings.TrimSpace(os.Getenv(envLogOutput))
	if len(cfg.OutputPaths) == 0 {
		if envVal != "" {
			cfg.OutputPaths = splitAndTrim(envVal)
		} else {
			cfg.OutputPaths = []string{"stdout"}
		}
	}
	if len(cfg.ErrorOutputPaths) == 0 {
		// Default error output matches output paths.
		cfg.ErrorOutputPaths = cfg.OutputPaths
	}
	return cfg
}

func splitAndTrim(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if v := strings.TrimSpace(p); v != "" {
			out = append(out, v)
		}
	}
	return out
}
