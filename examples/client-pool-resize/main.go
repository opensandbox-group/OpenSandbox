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

// Command client-pool-resize keeps a sandbox pool's idle target on a local
// wall-clock schedule. It is the smallest complete example of driving
// opensandbox.PoolResizeAdapter from a time-window policy.
//
// Run it against a reachable control plane:
//
//	OPEN_SANDBOX_DOMAIN=api.opensandbox.io \
//	OPEN_SANDBOX_API_KEY=... \
//	go run .
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	opensandbox "github.com/alibaba/OpenSandbox/sdks/sandbox/go"
)

const (
	// evalInterval is how often the policy is re-evaluated. One minute keeps the
	// example responsive without turning it into a busy loop.
	evalInterval = time.Minute

	// overnightIdle is the target between 22:00 and 06:00.
	overnightIdle = 0
	// businessIdle is the target between 09:00 and 18:00 on weekdays.
	businessIdle = 6
	// defaultIdle applies outside every window, including weekends.
	defaultIdle = 2
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	domain := os.Getenv("OPEN_SANDBOX_DOMAIN")
	if domain == "" {
		return errors.New("OPEN_SANDBOX_DOMAIN is required")
	}
	apiKey := os.Getenv("OPEN_SANDBOX_API_KEY")

	// A named business location, not time.Local: every process that shares this
	// pool namespace must agree on when a window opens.
	location, err := time.LoadLocation(envOr("OPEN_SANDBOX_TIMEZONE", "UTC"))
	if err != nil {
		return fmt.Errorf("load timezone: %w", err)
	}

	pool, err := opensandbox.NewSandboxPoolBuilder().
		PoolName(envOr("OPEN_SANDBOX_POOL_NAME", "resize-demo")).
		OwnerID(envOr("OPEN_SANDBOX_OWNER_ID", "resize-example")).
		MaxIdle(defaultIdle).
		ConnectionConfig(opensandbox.ConnectionConfig{
			Domain:   domain,
			APIKey:   apiKey,
			Protocol: "https",
		}).
		CreationSpec(opensandbox.PoolCreationSpec{Image: "ubuntu:22.04"}).
		// One process owns the schedule in this example. Share a state store
		// (Redis) instead when several processes resize the same namespace.
		StateStore(opensandbox.NewInMemoryPoolStateStore()).
		Build()
	if err != nil {
		return fmt.Errorf("build pool: %w", err)
	}

	// Start the pool before handing it to the adapter: Apply and Run return
	// ErrPoolResizePoolNotRunning until the pool is RUNNING.
	if err := pool.Start(ctx); err != nil {
		return err
	}
	defer func() {
		if err := pool.Shutdown(context.Background(), true); err != nil {
			log.Printf("pool shutdown failed: %v", err)
		}
	}()

	policy, err := opensandbox.NewTimeWindowPoolResizePolicy(
		[]opensandbox.PoolResizeWindow{
			{
				Name:    "overnight",
				Start:   22 * time.Hour,
				End:     6 * time.Hour,
				MaxIdle: overnightIdle,
			},
			{
				Name:  "business-hours",
				Start: 9 * time.Hour,
				End:   18 * time.Hour,
				// A weekday filter keeps the weekend on the default target.
				Weekdays: []time.Weekday{
					time.Monday, time.Tuesday, time.Wednesday,
					time.Thursday, time.Friday,
				},
				MaxIdle: businessIdle,
			},
		},
		location,
		defaultIdle,
		evalInterval,
	)
	if err != nil {
		return fmt.Errorf("build policy: %w", err)
	}

	adapter, err := opensandbox.NewPoolResizeAdapter(pool, policy)
	if err != nil {
		return fmt.Errorf("build adapter: %w", err)
	}
	log.Printf("starting the resize policy (timezone %s)", location)

	// Run evaluates the policy immediately, so the first window applies without
	// waiting out an interval.
	policyCtx, cancelPolicy := context.WithCancel(ctx)
	defer cancelPolicy()
	policyDone := make(chan struct{})
	policyResult := make(chan error, 1)
	go func() {
		defer close(policyDone)
		policyResult <- adapter.Run(policyCtx)
	}()

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			cancelPolicy()
			<-policyDone
			return nil

		case err := <-policyResult:
			cancelPolicy()
			<-policyDone
			if err != nil {
				return err
			}
			// Run returns nil once the pool is no longer resizable. Surface the
			// cause and exit non-zero: a namespace fenced by a peer deploy should
			// not look like a clean finish.
			if cause := adapter.LastError(); cause != nil {
				return fmt.Errorf("resize policy stopped: %w", cause)
			}
			log.Printf("resize policy stopped because the pool is no longer resizable")
			return nil

		case <-ticker.C:
			// Run retries a state-store outage on the next interval instead of
			// returning it, so surface it here rather than exiting quietly.
			if err := adapter.LastError(); err != nil {
				log.Printf("last resize error (will retry): %v", err)
			}
			snapshot, err := pool.Snapshot(ctx)
			if err != nil {
				log.Printf("snapshot failed: %v", err)
				continue
			}
			log.Printf("idle=%d target=%d state=%s", snapshot.IdleCount, snapshot.MaxIdle, snapshot.LifecycleState)
		}
	}
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
