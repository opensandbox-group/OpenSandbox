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

// Field is a structured logging key/value pair.
type Field struct {
	Key   string
	Value any
}

// Logger defines the minimal logging surface shared by components.
//   - Formatted levels: Debugf/Infof/Warnf/Errorf
//   - With: attach structured fields to derived logger
//   - Named: derive a sub-logger with name
//   - Sync: flush buffers (no-op for implementations that don't buffer)
type Logger interface {
	Debugf(template string, args ...any)
	Infof(template string, args ...any)
	Warnf(template string, args ...any)
	Errorf(template string, args ...any)
	With(fields ...Field) Logger
	Named(name string) Logger
	Sync() error
}
