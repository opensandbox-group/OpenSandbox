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

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"
	_ "time/tzdata"

	opensandbox "github.com/alibaba/OpenSandbox/sdks/sandbox/go"
)

func main() {
	zone := flag.String("timezone", "Asia/Shanghai", "IANA timezone for weekday 09:00-21:00 business hours")
	peak := flag.Int("peak-idle", 5, "idle target during business hours")
	offpeak := flag.Int("offpeak-idle", 0, "idle target outside business hours")
	interval := flag.Duration("interval", 30*time.Second, "schedule check interval")
	image := flag.String("image", "ubuntu:22.04", "sandbox image")
	flag.Parse()
	if *peak < 0 || *offpeak < 0 || *interval <= 0 {
		log.Fatal("idle targets must be nonnegative and interval must be positive")
	}
	location, err := time.LoadLocation(*zone)
	if err != nil {
		log.Fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, location, *peak, *offpeak, *interval, *image); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, location *time.Location, peak, offpeak int, interval time.Duration, image string) (err error) {
	initialTarget := desiredMaxIdle(time.Now(), location, peak, offpeak)
	pool, err := opensandbox.NewSandboxPoolBuilder().
		PoolName("business-hours-example").
		MaxIdle(initialTarget).
		ConnectionConfig(opensandbox.ConnectionConfig{UseServerProxy: true}).
		CreationSpec(opensandbox.PoolCreationSpec{
			Image:      image,
			Entrypoint: []string{"/bin/sh", "-c", "sleep infinity"},
		}).
		WarmupConcurrency(2).
		PrimaryLockTTL(2 * time.Minute).
		Build()
	if err != nil {
		return err
	}
	if err := pool.Start(ctx); err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, shutdownPool(pool))
	}()
	log.Printf("pool started: target=%d timezone=%s", initialTarget, location)
	return runSchedule(ctx, interval, func(now time.Time) error {
		changed, err := resizeForTime(ctx, pool, now, location, peak, offpeak)
		if err != nil {
			return err
		}
		if changed {
			log.Printf("idle target changed: target=%d local_time=%s",
				desiredMaxIdle(now, location, peak, offpeak), now.In(location).Format(time.RFC3339))
		}
		return nil
	})
}

func shutdownPool(pool *opensandbox.DefaultSandboxPool) error {
	// This example owns a single-process pool. Stop warmup before draining it,
	// and use a fresh context because Ctrl+C has canceled the scheduler context.
	cleanupCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	shutdownErr := pool.Shutdown(cleanupCtx, true)
	drained, drainErr := pool.ReleaseAllIdleParallel(cleanupCtx, 4)
	if drainErr == nil && drained > 0 {
		log.Printf("drained %d idle sandboxes", drained)
	}
	return errors.Join(shutdownErr, drainErr)
}

// desiredMaxIdle is application policy, separate from the SDK's reconciliation.
func desiredMaxIdle(now time.Time, location *time.Location, peak, offpeak int) int {
	local := now.In(location)
	weekday := local.Weekday()
	if weekday >= time.Monday && weekday <= time.Friday && local.Hour() >= 9 && local.Hour() < 21 {
		return peak
	}
	return offpeak
}

func resizeForTime(ctx context.Context, pool *opensandbox.DefaultSandboxPool, now time.Time, location *time.Location, peak, offpeak int) (bool, error) {
	snapshot, err := pool.Snapshot(ctx)
	if err != nil {
		return false, fmt.Errorf("read pool target: %w", err)
	}
	target := desiredMaxIdle(now, location, peak, offpeak)
	if snapshot.MaxIdle == target {
		return false, nil
	}
	if err := pool.Resize(ctx, target); err != nil {
		return false, fmt.Errorf("resize pool to %d: %w", target, err)
	}
	return true, nil
}

// runSchedule applies immediately, then re-evaluates wall-clock time on each tick.
// An error exits; recovery/leader election belongs to the hosting application.
func runSchedule(ctx context.Context, interval time.Duration, apply func(time.Time) error) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return nil
		}
		if err := apply(time.Now()); err != nil {
			if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
				return nil
			}
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
