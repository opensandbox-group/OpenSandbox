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

package opensandbox

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultPoolResizePolicyInterval = time.Minute
	poolResizeLocalDay              = 24 * time.Hour
)

// ErrPoolResizePoolNotRunning is returned when Apply or Run is called while
// the pool is NOT_STARTED or STARTING. The adapter remains reusable; start the
// pool and call it again. The returned error also unwraps to a
// *PoolNotRunningError, so callers that already handle the pool's typed errors
// keep working.
var ErrPoolResizePoolNotRunning = errors.New("opensandbox: pool resize adapter: pool is not running")

// resizeNotRunningError reports a pool that cannot be resized yet. It matches
// ErrPoolResizePoolNotRunning under errors.Is and unwraps to a
// *PoolNotRunningError under errors.As. PoolName is empty because the adapter
// only sees the PoolResizeTarget interface, not the pool's configuration.
type resizeNotRunningError struct {
	PoolNotRunningError
}

func (e *resizeNotRunningError) Error() string {
	// The adapter cannot see the pool's name, so omit the empty `pool ""`
	// clause the typed error would otherwise print.
	return fmt.Sprintf("opensandbox: pool resize adapter: pool is not running (state=%s)", e.State)
}

func (e *resizeNotRunningError) Is(target error) bool {
	return target == ErrPoolResizePoolNotRunning
}

func (e *resizeNotRunningError) Unwrap() error {
	return &e.PoolNotRunningError
}

// PoolResizeTarget is the part of a sandbox pool the resize adapter drives.
// *DefaultSandboxPool satisfies it; tests and external schedulers can supply
// their own implementation.
type PoolResizeTarget interface {
	// Resize persists a new idle target. It is the pool's existing contract and
	// remains the single writer of the target in the state store.
	Resize(ctx context.Context, maxIdle int) error
	// Snapshot returns the current pool state for policy evaluation.
	Snapshot(ctx context.Context) (*PoolSnapshot, error)
}

// PoolResizePolicy computes an idle target from a pool snapshot. Policies live
// outside the pool reconciler. An implementation only computes a target;
// PoolResizeAdapter applies that target through the existing
// SandboxPool.Resize method, so reconciliation, warmup limits, state-store
// coordination, and acquire behavior remain unchanged. Implementations must not
// call back into the adapter from NextMaxIdle.
type PoolResizePolicy interface {
	// NextMaxIdle returns the target for this evaluation. A negative value is
	// rejected by the adapter.
	NextMaxIdle(ctx context.Context, snapshot PoolSnapshot) (int, error)
	// Interval is how often Run re-evaluates the policy. It must be positive.
	Interval() time.Duration
}

// PoolResizeAdapter applies a PoolResizePolicy to a pool. Each evaluation reads
// a snapshot, asks the policy for a target, and writes it with Resize. A pool
// that is draining or stopped, or a namespace that has been destroyed,
// permanently stops the adapter. A NOT_STARTED or STARTING pool is a caller
// error (ErrPoolResizePoolNotRunning) and leaves the adapter reusable. The
// state store's destroy fence remains the final authority if a destroy races
// with a write.
type PoolResizeAdapter struct {
	pool   PoolResizeTarget
	policy PoolResizePolicy

	// applyMu serializes evaluations so concurrent Apply and Run calls do not
	// issue overlapping Resize writes. Stopped is atomic so status checks never
	// block behind store I/O.
	applyMu sync.Mutex
	stopped atomic.Bool

	errMu   sync.RWMutex
	lastErr error
}

// NewPoolResizeAdapter builds an adapter for pool using policy. The policy
// interval must be positive. Neither argument may be a nil pointer held in a
// non-nil interface.
func NewPoolResizeAdapter(pool PoolResizeTarget, policy PoolResizePolicy) (*PoolResizeAdapter, error) {
	if pool == nil {
		return nil, errors.New("opensandbox: pool resize adapter: pool is nil")
	}
	if policy == nil {
		return nil, errors.New("opensandbox: pool resize adapter: policy is nil")
	}
	if policy.Interval() <= 0 {
		return nil, fmt.Errorf("opensandbox: pool resize adapter: policy interval must be positive, got %s", policy.Interval())
	}
	return &PoolResizeAdapter{pool: pool, policy: policy}, nil
}

// Apply evaluates the policy once and calls Resize with the resulting target.
// It returns ErrPoolResizePoolNotRunning unless the pool is RUNNING, and is a
// no-op after the adapter has observed a stopped or destroyed pool.
func (a *PoolResizeAdapter) Apply(ctx context.Context) error {
	_, err := a.apply(ctx)
	return err
}

// Run evaluates the policy immediately and then on every policy interval until
// ctx is cancelled or the pool becomes stopped/destroyed. It returns
// ErrPoolResizePoolNotRunning if the pool is not yet RUNNING. Context
// cancellation is returned to the caller. A *PoolStateStoreUnavailableError is
// retried on the next interval; policy errors are returned immediately.
func (a *PoolResizeAdapter) Run(ctx context.Context) error {
	for {
		stopped, err := a.apply(ctx)
		if err != nil && !isPoolResizeRetryableError(err) {
			return err
		}
		if stopped {
			return nil
		}
		if err := a.wait(ctx); err != nil {
			return err
		}
	}
}

// Stopped reports whether the adapter has permanently stopped. It never blocks
// behind an in-flight Resize.
func (a *PoolResizeAdapter) Stopped() bool {
	return a.stopped.Load()
}

// LastError returns the most recent error seen by an evaluation. A successful
// evaluation clears it; a permanent stop records the cause that ended the
// adapter and keeps it. Run keeps retrying state-store unavailability without
// returning that error, so callers that need to surface the outage can poll this
// method or use Apply for per-call errors. The context error that ends Run is
// returned to the caller and is not recorded here.
func (a *PoolResizeAdapter) LastError() error {
	a.errMu.RLock()
	defer a.errMu.RUnlock()
	return a.lastErr
}

func (a *PoolResizeAdapter) setLastError(err error) {
	a.errMu.Lock()
	a.lastErr = err
	a.errMu.Unlock()
}

// stop marks the adapter permanently stopped and records why. It returns nil so
// the caller can report a terminal stop as `return true, a.stop(cause)`.
func (a *PoolResizeAdapter) stop(cause error) error {
	a.stopped.Store(true)
	a.setLastError(cause)
	return nil
}

// wait blocks for one policy interval or until ctx is done. The interval is
// re-read every iteration and floored, so a policy whose Interval changes or
// returns a non-positive value cannot turn Run into a busy loop.
func (a *PoolResizeAdapter) wait(ctx context.Context) error {
	interval := a.policy.Interval()
	if interval <= 0 {
		interval = defaultPoolResizePolicyInterval
	}
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (a *PoolResizeAdapter) apply(ctx context.Context) (stopped bool, err error) {
	a.applyMu.Lock()
	defer a.applyMu.Unlock()
	// A terminal stop is reported as a nil error so Apply and Run stay usable,
	// but the cause that ended the adapter is recorded instead of discarded:
	// otherwise a namespace fenced by a peer deploy is indistinguishable from a
	// local drain. A successful evaluation clears the field.
	defer func() {
		switch {
		case err != nil:
			a.setLastError(err)
		case !stopped:
			a.setLastError(nil)
		}
	}()

	// The stopped check comes first so a permanent stop is write-once: a later
	// call with a cancelled context must not overwrite the recorded cause with
	// context.Canceled.
	if a.stopped.Load() {
		return true, nil
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}

	snapshot, err := a.pool.Snapshot(ctx)
	if err != nil {
		if isPoolResizeTerminalError(err) {
			return true, a.stop(err)
		}
		return false, err
	}
	if snapshot == nil {
		return false, errors.New("opensandbox: pool resize adapter: pool returned a nil snapshot")
	}
	if snapshot.LifecycleState != PoolLifecycleRunning {
		if isPoolResizeTerminalState(snapshot.LifecycleState) {
			return true, a.stop(poolResizeTerminalStateError(snapshot.LifecycleState))
		}
		return false, poolResizeNotRunningError(snapshot.LifecycleState)
	}

	target, err := a.policy.NextMaxIdle(ctx, *snapshot)
	if err != nil {
		return false, fmt.Errorf("opensandbox: pool resize adapter: evaluate policy: %w", err)
	}
	if target < 0 {
		return false, fmt.Errorf("opensandbox: pool resize adapter: policy returned negative maxIdle: %d", target)
	}

	// Re-read the lifecycle immediately before the write. Snapshot is not fenced
	// by the state store, so this only narrows the local shutdown race; a
	// namespace retirement is observed by the Resize below, which is the
	// authoritative check.
	latest, err := a.pool.Snapshot(ctx)
	if err != nil {
		if isPoolResizeTerminalError(err) {
			return true, a.stop(err)
		}
		return false, err
	}
	if latest == nil {
		return false, errors.New("opensandbox: pool resize adapter: pool returned a nil snapshot")
	}
	if latest.LifecycleState != PoolLifecycleRunning {
		if isPoolResizeTerminalState(latest.LifecycleState) {
			return true, a.stop(poolResizeTerminalStateError(latest.LifecycleState))
		}
		return false, poolResizeNotRunningError(latest.LifecycleState)
	}

	// Resize is called on every evaluation, even when the target already matches
	// the snapshot. Skipping the write would hide a destroy that raced with the
	// evaluation, because the fence is only observed by a write attempt.
	if err := a.pool.Resize(ctx, target); err != nil {
		if isPoolResizeTerminalError(err) {
			return true, a.stop(err)
		}
		return false, err
	}
	return false, nil
}

func poolResizeTerminalStateError(state PoolLifecycleState) error {
	return fmt.Errorf("opensandbox: pool resize adapter: pool reached the terminal state %s", state)
}

func isPoolResizeTerminalState(state PoolLifecycleState) bool {
	return state == PoolLifecycleDraining || state == PoolLifecycleStopped
}

func poolResizeNotRunningError(state PoolLifecycleState) error {
	return &resizeNotRunningError{
		PoolNotRunningError: PoolNotRunningError{State: state},
	}
}

func isPoolResizeTerminalError(err error) bool {
	var destroyed *PoolDestroyedError
	return errors.As(err, &destroyed)
}

func isPoolResizeRetryableError(err error) bool {
	var unavailable *PoolStateStoreUnavailableError
	return errors.As(err, &unavailable)
}

// PoolResizeWindow is one local wall-clock window and the idle target that
// applies inside it. Start and End are offsets from local midnight. Start is
// inclusive and End is exclusive, so adjacent windows do not overlap. A window
// whose End is earlier than Start wraps past local midnight, which makes an
// overnight window a single entry rather than two. End equal to Start is
// rejected: it would otherwise be indistinguishable from a full-day window.
type PoolResizeWindow struct {
	// Name identifies the window in validation errors. It may be empty.
	Name string
	// Start is the inclusive window start as an offset from local midnight. It
	// must be in [0, 24h).
	Start time.Duration
	// End is the exclusive window end as an offset from local midnight. It must
	// be in (0, 24h] and must differ from Start.
	End time.Duration
	// MaxIdle is the idle target applied while the window is active.
	MaxIdle int
	// Weekdays optionally restricts the window to specific local weekdays. An
	// empty list matches every day. For a window that wraps midnight, Weekdays
	// names the day the window *starts* on: a Friday 22:00-06:00 window also
	// covers the small hours of Saturday.
	Weekdays []time.Weekday
}

// TimeWindowPoolResizePolicy selects an idle target from local wall-clock
// windows. The first matching window wins, so list specific windows before
// general ones. When no window matches, DefaultMaxIdle applies.
//
// A policy is evaluated in the configured Location, not the host location, so
// the same configuration produces the same schedule on every machine sharing a
// pool namespace.
type TimeWindowPoolResizePolicy struct {
	// Windows are evaluated in order; the first match wins.
	Windows []PoolResizeWindow
	// Location is the timezone the windows are expressed in. Nil means UTC.
	Location *time.Location
	// DefaultMaxIdle applies when no window matches.
	DefaultMaxIdle int

	// interval overrides the default evaluation period. It is unexported so a
	// struct literal stays valid; use NewTimeWindowPoolResizePolicy to set it.
	interval time.Duration
	// now is the clock, overridable in tests.
	now func() time.Time
}

// NewTimeWindowPoolResizePolicy validates windows and returns a policy that
// evaluates on every interval. It rejects a negative target, a window whose
// bounds fall outside a local day, and a weekday filter that is out of order or
// contains duplicates.
func NewTimeWindowPoolResizePolicy(windows []PoolResizeWindow, location *time.Location, defaultMaxIdle int, interval time.Duration) (*TimeWindowPoolResizePolicy, error) {
	if interval <= 0 {
		return nil, fmt.Errorf("opensandbox: pool resize policy: interval must be positive, got %s", interval)
	}
	if defaultMaxIdle < 0 {
		return nil, fmt.Errorf("opensandbox: pool resize policy: default maxIdle must be >= 0, got %d", defaultMaxIdle)
	}
	if location == nil {
		location = time.UTC
	}
	for i, w := range windows {
		if err := validatePoolResizeWindow(windows, i, w); err != nil {
			return nil, err
		}
	}
	return &TimeWindowPoolResizePolicy{
		Windows:        windows,
		Location:       location,
		DefaultMaxIdle: defaultMaxIdle,
		interval:       interval,
	}, nil
}

func validatePoolResizeWindow(windows []PoolResizeWindow, idx int, w PoolResizeWindow) error {
	if w.Start < 0 || w.Start >= poolResizeLocalDay {
		return fmt.Errorf("opensandbox: pool resize policy: window %d start %s is outside a local day", idx, w.Start)
	}
	if w.End <= 0 || w.End > poolResizeLocalDay {
		return fmt.Errorf("opensandbox: pool resize policy: window %d end %s is outside a local day", idx, w.End)
	}
	if w.End == w.Start {
		// Otherwise this reads as a wrap and silently matches the whole day.
		return fmt.Errorf("opensandbox: pool resize policy: window %d is empty: start and end are both %s", idx, w.Start)
	}
	if w.MaxIdle < 0 {
		return fmt.Errorf("opensandbox: pool resize policy: window %d maxIdle must be >= 0, got %d", idx, w.MaxIdle)
	}
	seen := make(map[time.Weekday]bool, len(w.Weekdays))
	for _, day := range w.Weekdays {
		if day < time.Sunday || day > time.Saturday {
			return fmt.Errorf("opensandbox: pool resize policy: window %d has an invalid weekday %d", idx, day)
		}
		if seen[day] {
			return fmt.Errorf("opensandbox: pool resize policy: window %d repeats weekday %s", idx, day)
		}
		seen[day] = true
	}
	// Two windows that are active at the same local time on the same day make
	// the result depend on declaration order, which is a silent
	// misconfiguration. Reject the overlap instead of picking a winner.
	for j := 0; j < idx; j++ {
		if poolResizeWindowsOverlap(windows[idx], windows[j]) {
			return fmt.Errorf("opensandbox: pool resize policy: window %d overlaps window %d on at least one weekday; the first match wins, so remove the ambiguity", idx, j)
		}
	}
	return nil
}

// poolResizeWindowSegment is a half-open [start, end) span of local wall-clock
// offsets active on one local day.
type poolResizeWindowSegment struct {
	start time.Duration
	end   time.Duration
}

// poolResizeWindowsOverlap reports whether two windows can be active at the same
// local wall-clock time on the same local day. Weekday filters and midnight
// wrapping are both honoured by comparing the actual segments each window
// occupies, so a pair the evaluator would resolve deterministically is never
// rejected.
func poolResizeWindowsOverlap(a, b PoolResizeWindow) bool {
	for day := time.Sunday; day <= time.Saturday; day++ {
		for _, as := range poolResizeSegmentsOn(a, day) {
			for _, bs := range poolResizeSegmentsOn(b, day) {
				if as.start < bs.end && bs.start < as.end {
					return true
				}
			}
		}
	}
	return false
}

// poolResizeSegmentsOn returns the spans w occupies on the given local day. A
// window that wraps midnight contributes two spans, and its post-midnight span
// belongs to the day after the one its Weekdays filter names.
func poolResizeSegmentsOn(w PoolResizeWindow, day time.Weekday) []poolResizeWindowSegment {
	if w.End > w.Start {
		if poolResizeWeekdayMatches(w.Weekdays, day) {
			return []poolResizeWindowSegment{{start: w.Start, end: w.End}}
		}
		return nil
	}
	var segments []poolResizeWindowSegment
	if poolResizeWeekdayMatches(w.Weekdays, day) {
		segments = append(segments, poolResizeWindowSegment{start: w.Start, end: poolResizeLocalDay})
	}
	if poolResizeWeekdayMatches(w.Weekdays, previousWeekday(day)) {
		segments = append(segments, poolResizeWindowSegment{start: 0, end: w.End})
	}
	return segments
}

func previousWeekday(day time.Weekday) time.Weekday {
	return (day + 6) % 7
}

// poolResizeWindowContains reports whether offset falls inside w, treating a
// window whose End is earlier than Start as wrapping past local midnight. It
// ignores the weekday filter; poolResizeSegmentsOn pairs the two.
func poolResizeWindowContains(w PoolResizeWindow, offset time.Duration) bool {
	if offset < 0 || offset >= poolResizeLocalDay {
		return false
	}
	if w.End > w.Start {
		return offset >= w.Start && offset < w.End
	}
	// Wrapped: [Start, 24h) union [0, End).
	return offset >= w.Start || offset < w.End
}

// NextMaxIdle returns the target of the first window that contains the current
// local time, or DefaultMaxIdle when none matches.
func (p *TimeWindowPoolResizePolicy) NextMaxIdle(_ context.Context, _ PoolSnapshot) (int, error) {
	now := p.clock().In(p.location())
	offset := poolResizeOffsetSinceMidnight(now)
	for _, w := range p.Windows {
		if !poolResizeWindowActiveOn(w, offset, now.Weekday(), now.AddDate(0, 0, -1).Weekday()) {
			continue
		}
		return w.MaxIdle, nil
	}
	return p.DefaultMaxIdle, nil
}

// poolResizeWindowActiveOn reports whether w is active at a local wall-clock
// offset. A window that wraps midnight belongs to the day it started on, so the
// post-midnight portion is matched against the previous weekday.
func poolResizeWindowActiveOn(w PoolResizeWindow, offset time.Duration, weekday, previousWeekday time.Weekday) bool {
	if !poolResizeWindowContains(w, offset) {
		return false
	}
	if w.End > w.Start || offset >= w.Start {
		return poolResizeWeekdayMatches(w.Weekdays, weekday)
	}
	// Past midnight in a wrapped window: the window started yesterday.
	return poolResizeWeekdayMatches(w.Weekdays, previousWeekday)
}

// Interval implements PoolResizePolicy.
func (p *TimeWindowPoolResizePolicy) Interval() time.Duration {
	if p.interval > 0 {
		return p.interval
	}
	return defaultPoolResizePolicyInterval
}

func (p *TimeWindowPoolResizePolicy) location() *time.Location {
	if p.Location != nil {
		return p.Location
	}
	return time.UTC
}

func (p *TimeWindowPoolResizePolicy) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

func poolResizeWeekdayMatches(filter []time.Weekday, day time.Weekday) bool {
	if len(filter) == 0 {
		return true
	}
	for _, candidate := range filter {
		if candidate == day {
			return true
		}
	}
	return false
}

// poolResizeOffsetSinceMidnight returns the local wall-clock offset of now.
//
// It is built from the calendar fields rather than by subtracting local
// midnight, because elapsed time is not wall-clock time on a daylight-saving
// day: on a 25-hour fall-back day "01:30" is two and a half hours after
// midnight, and "23:30" is more than a day after it. Reading the fields keeps a
// window pinned to the clock time an operator wrote down, so a 01:00-03:00
// window contains 01:30 on both sides of a transition.
func poolResizeOffsetSinceMidnight(now time.Time) time.Duration {
	offset := time.Duration(now.Hour())*time.Hour +
		time.Duration(now.Minute())*time.Minute +
		time.Duration(now.Second())*time.Second +
		time.Duration(now.Nanosecond())
	if offset >= poolResizeLocalDay {
		// Only reachable for an offset the clock cannot report; keep the value
		// inside the day so window lookup stays well defined.
		return poolResizeLocalDay - 1
	}
	return offset
}
