// Copyright 2026 The OpenSandbox Authors
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
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"go.uber.org/zap/zapcore"
)

// Outputs holds the physical destinations for one output owner. The owner
// serializes writes, syncs and closure across both routes.
type Outputs struct {
	Out, Errors zapcore.WriteSyncer
	closers     []io.Closer
	closeErr    error
}

// Close releases each acquired destination once. Borrowed stdout and stderr
// are not closed. Only the output owner may call Close.
func (o *Outputs) Close() error {
	for _, closer := range o.closers {
		if err := closer.Close(); o.closeErr == nil {
			o.closeErr = err
		}
	}
	o.closers = nil
	return o.closeErr
}

// OpenOutputs opens the configured destinations. Repeated and aliased paths
// remain separate logical copies, but share one physical writer across routes.
func OpenOutputs(cfg Config) (_ *Outputs, err error) {
	cfg = applyEnvOutputs(cfg)
	if len(cfg.OutputPaths) == 0 {
		// Only the ordinary route falls back to stdout. The default error
		// route was resolved before this fallback and remains empty.
		cfg.OutputPaths = []string{"stdout"}
	}
	var rotate RotateConfig
	if cfg.Rotate != nil {
		rotate = *cfg.Rotate
	}
	rotate.applyDefaults()

	outputs := &Outputs{}
	defer func() {
		if err != nil {
			if closeErr := outputs.Close(); closeErr != nil {
				err = errors.Join(err, closeErr)
			}
		}
	}()
	type destination struct {
		info   os.FileInfo
		writer zapcore.WriteSyncer
	}
	var destinations []destination
	byPath := make(map[string]zapcore.WriteSyncer)
	open := func(path string) (zapcore.WriteSyncer, error) {
		file, owned, err := openOutputPath(path)
		if err != nil {
			return nil, err
		}
		info, err := file.Stat()
		if err != nil && owned {
			return nil, errors.Join(err, file.Close())
		}
		for _, existing := range destinations {
			if info != nil && existing.info != nil && os.SameFile(info, existing.info) {
				if owned {
					if err := file.Close(); err != nil {
						return nil, err
					}
				}
				return existing.writer, nil
			}
		}

		var writer zapcore.WriteSyncer
		if owned && info.Mode().IsRegular() {
			if name := rotationPath(path, info); name != "" {
				if err := file.Close(); err != nil {
					return nil, err
				}
				rotating := rotatingFile(name, &rotate)
				outputs.closers = append(outputs.closers, rotating)
				writer = zapcore.AddSync(rotating)
			}
		}
		if writer == nil {
			if owned {
				outputs.closers = append(outputs.closers, file)
			}
			// Streams and opaque descriptors have no durability Sync here.
			writer = zapcore.AddSync(struct{ io.Writer }{file})
		}
		destinations = append(destinations, destination{info: info, writer: writer})
		return writer, nil
	}
	route := func(paths []string) (zapcore.WriteSyncer, error) {
		if len(paths) == 0 {
			return zapcore.AddSync(io.Discard), nil
		}
		sinks := make([]zapcore.WriteSyncer, 0, len(paths))
		for _, path := range paths {
			writer, ok := byPath[path]
			if !ok {
				var err error
				writer, err = open(path)
				if err != nil {
					return nil, fmt.Errorf("invalid output path %q: %w", path, err)
				}
				byPath[path] = writer
			}
			sinks = append(sinks, writer)
		}
		return zapcore.NewMultiWriteSyncer(sinks...), nil
	}
	if outputs.Out, err = route(cfg.OutputPaths); err != nil {
		return nil, err
	}
	if outputs.Errors, err = route(cfg.ErrorOutputPaths); err != nil {
		return nil, err
	}
	return outputs, nil
}

func openOutputPath(path string) (*os.File, bool, error) {
	switch path {
	case "stdout":
		return os.Stdout, false, nil
	case "stderr":
		return os.Stderr, false, nil
	default:
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0644)
		return file, true, err
	}
}

// rotationPath returns a usable on-disk name for the already-open file. In
// particular a procfs magic link can point to an unlinked file even if its
// displayed target does not exist or refers to a different file.
func rotationPath(path string, opened os.FileInfo) string {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return ""
	}
	name, err := filepath.Abs(resolved)
	if err != nil {
		return ""
	}
	info, err := os.Stat(name)
	if err != nil || !os.SameFile(info, opened) {
		return ""
	}
	return name
}

// NewWithWriter builds the same filtered and sampled JSON logger as New, but
// delegates byte admission, flushing and lifetime to the supplied writers.
func NewWithWriter(cfg Config, out, errs zapcore.WriteSyncer) Logger {
	if len(applyEnvOutputs(cfg).OutputPaths) == 0 {
		return buildLogger(fallbackCore(out), errs)
	}
	return buildLogger(zapcore.NewCore(productionEncoder(), out, parseLevel(cfg.Level)), errs)
}
