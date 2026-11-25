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
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/log"
	"github.com/alibaba/opensandbox/egress/pkg/telemetry"
	"github.com/alibaba/opensandbox/internal/safego"
)

type Runner func(context.Context, string) ([]byte, error)
type Binder func(context.Context, Config, func(Packet), func(error)) (func(), error)

// Runtime serializes counter reads with table replacement, including the
// final pre-replacement read and explicit baseline reset after success.
type Runtime struct {
	config Config
	table  string
	mu     sync.Mutex
	last   map[string]uint64
	read   func(context.Context) (map[string]uint64, error)
	record func(Reason, uint64)
}

var errCounterTableMissing = errors.New("drop counter table is absent")

func New(c Config, table string) *Runtime {
	r := &Runtime{config: c, table: table, last: make(map[string]uint64), record: func(reason Reason, n uint64) { telemetry.RecordBlockedPackets(string(reason), n) }}
	r.read = func(ctx context.Context) (map[string]uint64, error) {
		out, e := exec.CommandContext(ctx, "nft", "-j", "list", "counters", "table", "inet", table).CombinedOutput()
		if e != nil {
			if strings.Contains(string(out), "No such file or directory") {
				return nil, errCounterTableMissing
			}
			return nil, fmt.Errorf("read drop counters: %w: %s", e, out)
		}
		return ParseCounters(out)
	}
	return r
}
func ParseCounters(data []byte) (map[string]uint64, error) {
	var doc struct {
		NFTables []struct {
			Counter *struct {
				Name    string `json:"name"`
				Packets uint64 `json:"packets"`
			} `json:"counter"`
		} `json:"nftables"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	out := make(map[string]uint64)
	for _, item := range doc.NFTables {
		if item.Counter != nil {
			if _, ok := reasonForCounter(item.Counter.Name); ok {
				out[item.Counter.Name] = item.Counter.Packets
			}
		}
	}
	return out, nil
}
func (r *Runtime) pollLocked(ctx context.Context) {
	counts, err := r.read(ctx)
	if err != nil {
		if errors.Is(err, errCounterTableMissing) {
			return
		}
		log.Warnf("drop telemetry: %v", err)
		return
	}
	for name, n := range counts {
		old := r.last[name]
		delta := n
		if n >= old {
			delta = n - old
		}
		if delta > 0 {
			reason, _ := reasonForCounter(name)
			r.record(reason, delta)
		}
	}
	r.last = counts
}
func (r *Runtime) Poll(ctx context.Context) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.config.Mode == Off || r.config.Mode == "" {
		return
	}
	r.pollLocked(ctx)
}

type replacementContextKey struct{}

// BeginReplacement reads counters before lease planning and excludes periodic
// reads until finish. Only use the returned context within that guarded scope.
func (r *Runtime) BeginReplacement(ctx context.Context) (context.Context, func()) {
	if r == nil {
		return ctx, func() {}
	}
	if ctx.Value(replacementContextKey{}) == r {
		return ctx, func() {}
	}
	r.mu.Lock()
	if r.config.Mode == Off || r.config.Mode == "" {
		r.mu.Unlock()
		return ctx, func() {}
	}
	readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	r.pollLocked(readCtx)
	cancel()
	return context.WithValue(ctx, replacementContextKey{}, r), sync.OnceFunc(r.mu.Unlock)
}

// Replacement callers must acquire BeginReplacement before rendering their script.
func (r *Runtime) Wrap(run Runner, reasonFor func(string) Reason) Runner {
	if r == nil {
		return run
	}
	return func(ctx context.Context, script string) ([]byte, error) {
		if ctx.Value(replacementContextKey{}) != r {
			r.mu.Lock()
			defer r.mu.Unlock()
		}
		if r.config.Mode == Off || r.config.Mode == "" {
			return run(ctx, script)
		}
		rendered, err := Instrument(script, r.config, reasonFor)
		if err != nil {
			return nil, err
		}
		replace := hasTableStatement(script, "delete", r.table)
		create := hasTableStatement(script, "add", r.table)
		out, err := run(ctx, rendered)
		if err == nil && (create || replace) {
			r.last = make(map[string]uint64)
		}
		return out, err
	}
}

// Start binds before probing nft support. Failed sampling never changes the
// enforcement transaction: subsequent scripts are rendered in counter mode.
func (r *Runtime) Start(ctx context.Context, run Runner, bind Binder, onPacket func(Packet)) func() {
	if r == nil {
		return func() {}
	}
	r.mu.Lock()
	cfg := r.config
	r.mu.Unlock()
	if cfg.Mode == Off || cfg.Mode == "" {
		return func() {}
	}
	stop := func() {}
	stopped := false
	if cfg.Mode == Sample {
		closeNF, err := bind(ctx, cfg, func(p Packet) {
			r.mu.Lock()
			if !stopped && r.config.Mode == Sample && ctx.Err() == nil {
				telemetry.SetDropSampler(true, "")
			}
			r.mu.Unlock()
			onPacket(p)
		}, func(err error) {
			telemetry.SetDropSampler(false, "receive")
			log.Warnf("drop telemetry receiver: %v", err)
		})
		cause := "bind"
		if err == nil {
			cause = "ruleset"
			probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			probe := fmt.Sprintf("add table inet opensandbox_drop_probe\nadd counter inet opensandbox_drop_probe probe\nadd chain inet opensandbox_drop_probe probe\nadd rule inet opensandbox_drop_probe probe limit rate %s burst %d packets counter name probe log group %d snaplen 128 prefix \"osb:probe\"\ndelete table inet opensandbox_drop_probe\n", cfg.Rate, cfg.Burst, cfg.Group)
			_, err = run(probeCtx, probe)
			cancel()
		}
		if err != nil {
			if closeNF != nil {
				closeNF()
			}
			r.mu.Lock()
			r.config.Mode = Counter
			r.mu.Unlock()
			telemetry.SetDropSampler(false, cause)
			log.Warnf("drop telemetry: sampling disabled (cause=%s): %v", cause, err)
		} else {
			stop = closeNF
			telemetry.SetDropSampler(true, "")
		}
	} else {
		telemetry.SetDropSampler(false, "disabled")
	}
	safego.Go(func() {
		ticker := time.NewTicker(cfg.Poll)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				r.mu.Lock()
				if !stopped {
					stopped = true
					telemetry.SetDropSampler(false, "disabled")
				}
				r.mu.Unlock()
				return
			case <-ticker.C:
				pollCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				r.Poll(pollCtx)
				cancel()
			}
		}
	})
	return sync.OnceFunc(func() {
		r.mu.Lock()
		stopped = true
		telemetry.SetDropSampler(false, "disabled")
		r.mu.Unlock()
		if stop != nil {
			stop()
		}
	})
}

func hasTableStatement(script, verb, table string) bool {
	for _, line := range strings.Split(script, "\n") {
		f := strings.Fields(line)
		if len(f) >= 4 && f[0] == verb && f[1] == "table" && f[2] == "inet" && f[3] == table {
			return true
		}
	}
	return false
}
