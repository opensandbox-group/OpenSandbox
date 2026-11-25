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

package droptelemetry

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/log"
	slogger "github.com/alibaba/opensandbox/internal/logger"
	"github.com/stretchr/testify/require"
)

func TestConfig(t *testing.T) {
	c, e := Parse(func(string) string { return "" })
	require.NoError(t, e)
	require.Equal(t, DefaultConfig(), c)
	for _, tc := range []struct{ key, value string }{{EnvMode, "bad"}, {EnvGroup, "65536"}, {EnvGroup, "-1"}, {EnvRate, "20/second; drop"}, {EnvRate, "4294967296/second"}, {EnvBurst, "0"}, {EnvPoll, "0s"}, {EnvPoll, "nonsense"}} {
		t.Run(tc.key+tc.value, func(t *testing.T) {
			_, e := Parse(func(k string) string {
				if k == tc.key {
					return tc.value
				}
				if k == EnvMode {
					return "sample"
				}
				return ""
			})
			require.Error(t, e)
		})
	}
	c, e = Parse(func(k string) string {
		if k == EnvGroup {
			return "bad"
		}
		return ""
	})
	require.NoError(t, e)
	require.Equal(t, Off, c.Mode)
}
func TestCounterDeltasAndReplacement(t *testing.T) {
	c := DefaultConfig()
	c.Mode = Counter
	r := New(c, "test")
	values := map[string]uint64{"deny_set": 10, "dot": 2}
	var total uint64
	var order []string
	r.read = func(context.Context) (map[string]uint64, error) { order = append(order, "read"); return values, nil }
	r.record = func(_ Reason, n uint64) { total += n }
	r.Poll(context.Background())
	require.EqualValues(t, 12, total)
	values = map[string]uint64{"deny_set": 15, "dot": 1}
	r.Poll(context.Background())
	require.EqualValues(t, 18, total)
	values = map[string]uint64{"deny_set": 18, "dot": 1}
	run := r.Wrap(func(context.Context, string) ([]byte, error) { order = append(order, "apply"); return nil, nil }, func(string) Reason { return DefaultDeny })
	ctx, finish := r.BeginReplacement(context.Background())
	_, e := run(ctx, "delete table inet test\nadd table inet test\n")
	finish()
	require.NoError(t, e)
	require.Equal(t, []string{"read", "read", "read", "apply"}, order)
	require.EqualValues(t, 21, total)
	values = map[string]uint64{"deny_set": 20}
	r.Poll(context.Background())
	require.EqualValues(t, 41, total, "explicit reset catches new value above old baseline")
	// Failed transactions retain the prior baseline, including the pre-read.
	fail := r.Wrap(func(context.Context, string) ([]byte, error) { return nil, errors.New("failed") }, nil)
	ctx, finish = r.BeginReplacement(context.Background())
	_, e = fail(ctx, "delete table inet test\nadd table inet test\n")
	finish()
	require.Error(t, e)
	values = map[string]uint64{"deny_set": 22}
	r.Poll(context.Background())
	require.EqualValues(t, 43, total)
	r.read = func(context.Context) (map[string]uint64, error) { return nil, errors.New("read failed") }
	r.Poll(context.Background())
	require.EqualValues(t, 43, total)
}
func TestProbeFallbackOrder(t *testing.T) {
	for _, cause := range []string{"bind", "ruleset", "success"} {
		t.Run(cause, func(t *testing.T) {
			c := DefaultConfig()
			c.Mode = Sample
			c.Poll = time.Hour
			r := New(c, "test")
			var steps []string
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			closeNF := r.Start(ctx, func(_ context.Context, s string) ([]byte, error) {
				steps = append(steps, "ruleset")
				require.Contains(t, s, "limit rate 20/second")
				require.Contains(t, s, "counter name probe")
				require.Contains(t, s, "log group 3085")
				require.True(t, strings.HasSuffix(s, "delete table inet opensandbox_drop_probe\n"))
				if cause == "ruleset" {
					return nil, errors.New("ruleset")
				}
				return nil, nil
			}, func(context.Context, Config, func(Packet), func(error)) (func(), error) {
				steps = append(steps, "bind")
				if cause == "bind" {
					return nil, errors.New("bind")
				}
				return func() { steps = append(steps, "close") }, nil
			}, nil)
			if cause == "success" {
				require.Equal(t, Sample, r.config.Mode)
				require.Equal(t, []string{"bind", "ruleset"}, steps)
			} else {
				require.Equal(t, Counter, r.config.Mode)
				if cause == "bind" {
					require.Equal(t, []string{"bind"}, steps)
				} else {
					require.Equal(t, []string{"bind", "ruleset", "close"}, steps)
				}
			}
			closeNF()
		})
	}
}
func TestParseCounters(t *testing.T) {
	counts, e := ParseCounters([]byte(`{"nftables":[{"metainfo":{}},{"counter":{"name":"deny_set","packets":9}},{"counter":{"name":"osb_123_subject_input_deny","packets":3}},{"counter":{"name":"other","packets":4}}]}`))
	require.NoError(t, e)
	require.Equal(t, map[string]uint64{"deny_set": 9, "osb_123_subject_input_deny": 3}, counts)
	_, e = ParseCounters([]byte("bad"))
	require.Error(t, e)
}
func TestInstrumentOwnershipAndUnmapped(t *testing.T) {
	c := DefaultConfig()
	c.Mode = Sample
	s := "add table inet test\nadd chain inet test subj_a\nadd rule inet test subj_a drop\n"
	got, e := Instrument(s, c, func(string) Reason { return DefaultDeny })
	require.NoError(t, e)
	require.Equal(t, len(reasons)*2, strings.Count(got, "add counter"))
	require.Contains(t, got, "counter name \"osb_")
	got, e = Instrument("flush chain inet test subj_a\nadd rule inet test subj_a drop\n", c, func(string) Reason { return DefaultDeny })
	require.NoError(t, e)
	require.NotContains(t, got, "add counter")
	_, e = Instrument(s, c, func(string) Reason { return "" })
	require.ErrorContains(t, e, "unmapped drop rule")
}

type dropWarningCapture struct {
	slogger.Logger
	warnings []string
}

func (l *dropWarningCapture) Warnf(format string, args ...any) {
	l.warnings = append(l.warnings, fmt.Sprintf(format, args...))
}
func TestMissingCounterTableIsQuiet(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nft")
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\necho 'Error: No such file or directory' >&2\nexit 1\n"), 0700))
	t.Setenv("PATH", dir)
	prior := log.Logger
	capture := &dropWarningCapture{Logger: prior}
	log.Logger = capture
	t.Cleanup(func() { log.Logger = prior })
	cfg := DefaultConfig()
	cfg.Mode = Counter
	r := New(cfg, "test")
	r.last = map[string]uint64{"deny_set": 7}
	r.Poll(context.Background())
	require.Empty(t, capture.warnings)
	require.Equal(t, map[string]uint64{"deny_set": 7}, r.last, "missing observation must not advance the baseline")
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\necho 'Error: Operation not permitted' >&2\nexit 1\n"), 0700))
	r.Poll(context.Background())
	require.Len(t, capture.warnings, 1)
	require.Contains(t, capture.warnings[0], "Operation not permitted")
}
func TestPreReplacementReadHasIndependentDeadline(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Mode = Counter
	r := New(cfg, "test")
	applyCtx, cancel := context.WithCancel(context.Background())
	cancel()
	r.read = func(ctx context.Context) (map[string]uint64, error) {
		require.NoError(t, ctx.Err(), "counter read must not inherit an expired apply context")
		deadline, ok := ctx.Deadline()
		require.True(t, ok)
		remaining := time.Until(deadline)
		require.Greater(t, remaining, time.Second)
		require.LessOrEqual(t, remaining, 2*time.Second)
		return map[string]uint64{}, nil
	}
	apply := r.Wrap(func(ctx context.Context, _ string) ([]byte, error) {
		require.ErrorIs(t, ctx.Err(), context.Canceled)
		return nil, ctx.Err()
	}, nil)
	readCtx, finish := r.BeginReplacement(applyCtx)
	defer finish()
	_, err := apply(readCtx, "delete table inet test\n")
	require.ErrorIs(t, err, context.Canceled)
}
func TestInstrumentTableFlagsDeclaresCounters(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Mode = Counter
	got, err := Instrument("add table inet test { flags owner; }\nadd chain inet test egress\nadd rule inet test egress drop\n", cfg, func(string) Reason { return DefaultDeny })
	require.NoError(t, err)
	require.Equal(t, len(reasons), strings.Count(got, "add counter inet test"))
	require.Less(t, strings.Index(got, "add counter"), strings.Index(got, "add rule"))
}

func TestReplacementExcludesPollingUntilCommit(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Mode = Counter
	r := New(cfg, "test")
	var order []string
	var packets, total uint64 = 10, 0
	r.read = func(context.Context) (map[string]uint64, error) {
		order = append(order, "read")
		return map[string]uint64{"deny_set": packets}, nil
	}
	r.record = func(_ Reason, n uint64) { total += n }
	ctx, finish := r.BeginReplacement(context.Background())
	defer func() {
		if finish != nil {
			finish()
		}
	}()
	started, done := make(chan struct{}), make(chan struct{})
	go func() { close(started); r.Poll(context.Background()); close(done) }()
	<-started
	select {
	case <-done:
		t.Fatal("periodic read interleaved with replacement planning")
	case <-time.After(20 * time.Millisecond):
	}
	apply := r.Wrap(func(context.Context, string) ([]byte, error) {
		order = append(order, "apply")
		packets = 12
		return nil, nil
	}, nil)
	_, err := apply(ctx, "delete table inet test\nadd table inet test\n")
	require.NoError(t, err)
	finish()
	finish = nil
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("polling did not resume after commit")
	}
	require.Equal(t, []string{"read", "apply", "read"}, order)
	require.EqualValues(t, 22, total, "successful replacement resets the counter baseline")
}
