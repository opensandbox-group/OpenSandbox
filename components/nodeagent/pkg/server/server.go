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

// Package server exposes the Node Agent health and readiness HTTP endpoints
// plus an optional loopback-only pprof server.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/pprof"
	"sort"
	"sync"
	"time"
)

// Readiness tracks the set of reasons the process is currently not ready.
// Reasons are free-form labels reported on /readyz.
type Readiness struct {
	mu      sync.RWMutex
	reasons map[string]struct{}
}

// NewReadiness returns a Readiness that starts not-ready with reason
// "starting".
func NewReadiness() *Readiness {
	return &Readiness{reasons: map[string]struct{}{"starting": {}}}
}

// Set adds reason to the not-ready set while active and removes it otherwise.
// Concurrent reasons combine; the process is ready only when the set is
// empty.
func (r *Readiness) Set(reason string, active bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if active {
		r.reasons[reason] = struct{}{}
	} else {
		delete(r.reasons, reason)
	}
}

// Replace discards every pending reason and marks the process not-ready with
// exactly reason. Use it for permanent conditions that make all other reasons
// irrelevant.
func (r *Readiness) Replace(reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reasons = map[string]struct{}{reason: {}}
}

// Ready reports whether the process is ready and, when not, the sorted set of
// blocking reasons.
func (r *Readiness) Ready() (bool, []string) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	reasons := make([]string, 0, len(r.reasons))
	for reason := range r.reasons {
		reasons = append(reasons, reason)
	}
	sort.Strings(reasons)
	return len(reasons) == 0, reasons
}

// Servers holds the running health and pprof HTTP servers.
type Servers struct {
	health *http.Server
	pprof  *http.Server
}

// Start serves /healthz and /readyz on addr and, when pprofAddr is non-empty,
// the pprof handlers on pprofAddr. Servers are started in the background;
// listen failures are reported through onError.
func Start(addr, pprofAddr string, readiness *Readiness, onError func(error)) *Servers {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"healthy":true}`))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		ready, reasons := readiness.Ready()
		w.Header().Set("Content-Type", "application/json")
		if !ready {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ready": ready, "reasons": reasons})
	})
	servers := &Servers{health: &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}}
	go serve(servers.health, "health", onError)
	if pprofAddr != "" {
		debugMux := http.NewServeMux()
		debugMux.HandleFunc("/debug/pprof/", pprof.Index)
		debugMux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		debugMux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		debugMux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		debugMux.HandleFunc("/debug/pprof/trace", pprof.Trace)
		servers.pprof = &http.Server{Addr: pprofAddr, Handler: debugMux, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second}
		go serve(servers.pprof, "pprof", onError)
	}
	return servers
}

// Shutdown gracefully stops both servers within ctx.
func (s *Servers) Shutdown(ctx context.Context) error {
	err := s.health.Shutdown(ctx)
	if s.pprof != nil {
		err = errors.Join(err, s.pprof.Shutdown(ctx))
	}
	return err
}

func serve(server *http.Server, name string, onError func(error)) {
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) && onError != nil {
		onError(fmt.Errorf("%s server on %s stopped: %w", name, server.Addr, err))
	}
}
