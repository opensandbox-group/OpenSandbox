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

package log

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func TestCommandIDFormattedCompatibility(t *testing.T) {
	var original, correlated bytes.Buffer
	makeLogger := func(sink *bytes.Buffer) *zap.Logger {
		core := zapcore.NewCore(zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()), zapcore.AddSync(sink), zap.InfoLevel)
		return zap.New(zapcore.NewSamplerWithOptions(core, time.Hour, 2, 3))
	}
	baseline, native := makeLogger(&original), makeLogger(&correlated)
	previous := current
	current = &loggerState{native: native}
	t.Cleanup(func() { current = previous })
	for i := 0; i < 12; i++ {
		for _, level := range []zapcore.Level{zap.InfoLevel, zap.WarnLevel, zap.ErrorLevel} {
			for _, input := range []struct {
				format string
				args   []any
			}{
				{"command: received %v", nil},
				{"command: received %v", []any{"payload"}},
				{"command: received %v", []any{i % 2}},
				{"", []any{"empty-format-payload"}},
				{"", []any{1, 2}},
				{"", nil},
			} {
				format, args := input.format, input.args
				switch level {
				case zap.InfoLevel:
					baseline.Sugar().Infof(format, args...)
					InfoWithCommandID(fmt.Sprint(i), format, args...)
				case zap.WarnLevel:
					baseline.Sugar().Warnf(format, args...)
					WarnWithCommandID(fmt.Sprint(i), format, args...)
				case zap.ErrorLevel:
					baseline.Sugar().Errorf(format, args...)
					ErrorWithCommandID(fmt.Sprint(i), format, args...)
				}
			}
		}
	}
	baseline.Sugar().Infof("noncommand %% unchanged")
	InfoWithCommandID("", "noncommand %% unchanged")
	decode := func(s string) []map[string]any {
		var records []map[string]any
		for _, line := range strings.Split(strings.TrimSpace(s), "\n") {
			var record map[string]any
			require.NoError(t, json.Unmarshal([]byte(line), &record))
			delete(record, "ts")
			records = append(records, record)
		}
		return records
	}
	expected, actual := decode(original.String()), decode(correlated.String())
	require.Len(t, actual, len(expected))
	for i, record := range actual {
		if i != len(actual)-1 {
			require.Contains(t, record, "command_id")
			delete(record, "command_id")
		}
		require.Equal(t, expected[i], record, "only command_id may differ; sampling and formatted messages must match")
	}
}
