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

package slotsource

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// FileParser parses a fastlet slot-store record ("Consumed slot fields" of the
// multi-sandbox egress control plane proposal)
// ("Consumed slot fields"). It is strict: any unknown phase, missing required
// field, or unparseable address is an error. The consumer must fail closed on
// parse errors — an unparsed slot is never treated as active. Fields outside
// the documented set are ignored; a future fastlet format change that keeps
// these fields compatible parses cleanly, and one that does not fails closed
// instead of mis-parsing.
//
// Expected shape (subset):
//
//	{
//	  "id": "slot-id",
//	  "phase": "Bound",
//	  "owner": {"sandboxUid": "uid", "instanceGeneration": 1, "assignmentAttempt": 2},
//	  "ip": "10.0.0.5",
//	  "hostNetnsPath": "/var/run/netns/ns",
//	  "hostVeth": "vethX",
//	  "gateway": "10.0.0.1",
//	  "privateCidr": "10.0.0.0/24",
//	  "dnsPath": "/run/fast-sandbox/network/dns/uid"
//	}
type FileParser struct{}

// rawSlot is the wire shape of a slot record. Extra fields are ignored.
type rawSlot struct {
	ID    string `json:"id"`
	Phase string `json:"phase"`
	Owner struct {
		SandboxUID         string `json:"sandboxUid"`
		InstanceGeneration uint64 `json:"instanceGeneration"`
		AssignmentAttempt  uint64 `json:"assignmentAttempt"`
	} `json:"owner"`
	IP            string `json:"ip"`
	HostNetnsPath string `json:"hostNetnsPath"`
	HostVeth      string `json:"hostVeth"`
	Gateway       string `json:"gateway"`
	PrivateCidr   string `json:"privateCidr"`
	DNSPath       string `json:"dnsPath"`
}

// Parse converts one raw slot record into the normalized model. Identity
// fields (id, owner.sandboxUid, phase) are required for every record; the
// network fields are required for bound records only, so teardown-phase
// records parse even when their network data is already gone.
func (FileParser) Parse(raw []byte) (Slot, error) {
	var r rawSlot
	if err := json.Unmarshal(raw, &r); err != nil {
		return Slot{}, fmt.Errorf("slot record is not valid JSON: %w", err)
	}
	if strings.TrimSpace(r.ID) == "" {
		return Slot{}, fmt.Errorf("slot record missing id")
	}
	if strings.TrimSpace(r.Owner.SandboxUID) == "" {
		return Slot{}, fmt.Errorf("slot record %q missing owner.sandboxUid", r.ID)
	}

	phase, err := parsePhase(r.Phase)
	if err != nil {
		return Slot{}, fmt.Errorf("slot record %q: %w", r.ID, err)
	}

	slot := Slot{
		ID:    r.ID,
		Phase: phase,
		Owner: Owner{
			SandboxUID:         r.Owner.SandboxUID,
			InstanceGeneration: r.Owner.InstanceGeneration,
			AssignmentAttempt:  r.Owner.AssignmentAttempt,
		},
	}
	if phase != PhaseBound {
		return slot, nil
	}

	ip, err := netip.ParseAddr(strings.TrimSpace(r.IP))
	if err != nil {
		return Slot{}, fmt.Errorf("slot record %q: invalid ip %q: %w", r.ID, r.IP, err)
	}
	gateway, err := netip.ParseAddr(strings.TrimSpace(r.Gateway))
	if err != nil {
		return Slot{}, fmt.Errorf("slot record %q: invalid gateway %q: %w", r.ID, r.Gateway, err)
	}
	slot.IP = ip
	slot.Gateway = gateway
	if strings.TrimSpace(r.HostNetnsPath) == "" {
		return Slot{}, fmt.Errorf("slot record %q missing hostNetnsPath", r.ID)
	}
	if strings.TrimSpace(r.HostVeth) == "" {
		return Slot{}, fmt.Errorf("slot record %q missing hostVeth", r.ID)
	}
	if strings.TrimSpace(r.DNSPath) == "" {
		return Slot{}, fmt.Errorf("slot record %q missing dnsPath", r.ID)
	}
	slot.HostNetnsPath = r.HostNetnsPath
	slot.HostVeth = r.HostVeth
	slot.DNSPath = r.DNSPath

	if cidr := strings.TrimSpace(r.PrivateCidr); cidr != "" {
		prefix, err := netip.ParsePrefix(cidr)
		if err != nil {
			return Slot{}, fmt.Errorf("slot record %q: invalid privateCidr %q: %w", r.ID, cidr, err)
		}
		slot.PrivateCIDR = prefix
	}
	return slot, nil
}

func parsePhase(raw string) (Phase, error) {
	switch Phase(raw) {
	case PhaseBound:
		return PhaseBound, nil
	case PhaseClean:
		return PhaseClean, nil
	case PhaseDestroying:
		return PhaseDestroying, nil
	default:
		return "", fmt.Errorf("unknown phase %q", raw)
	}
}

// defaultPollInterval is the slot-store polling interval when none is set.
const defaultPollInterval = time.Second

// FileSource is a polling Source over a directory of slot-store JSON files.
// Polling (rather than fsnotify) matches the fleet profile scaling note (inotify
// watch limits on shared hosts with polling fallback) and keeps the watcher
// dependency-free; fsnotify can be swapped in later without touching the
// Source contract.
//
// Only bound records are emitted as Bound/Updated; Clean/Destroying records
// are treated as gone (a teardown-phase file is one deletion away from being
// gone, and fastlet's release path deletes the file anyway).
type FileSource struct {
	dir      string
	interval time.Duration
}

// NewFileSource watches dir for slot-store JSON files. interval <= 0 selects
// the default 1s poll.
func NewFileSource(dir string, interval time.Duration) *FileSource {
	if interval <= 0 {
		interval = defaultPollInterval
	}
	return &FileSource{dir: dir, interval: interval}
}

var _ Source = (*FileSource)(nil)

// List returns all currently bound slots. A single unparseable record fails
// the whole snapshot (fail closed): starting with a partial view would leave
// the remaining sandboxes without enforcement.
func (s *FileSource) List(ctx context.Context) ([]Slot, error) {
	slots, _, err := s.readDir()
	if err != nil {
		return nil, err
	}
	out := make([]Slot, 0, len(slots))
	for _, slot := range slots {
		if slot.Phase == PhaseBound {
			out = append(out, slot)
		}
	}
	return out, nil
}

// Watch streams lifecycle events by diffing successive directory snapshots.
// Events are delivered in order per slot; a create+delete between polls is
// invisible (the transient sandbox is harmless — it was never enforced as
// active, and its traffic was dropped by the master chain). Parse failures of
// individual records are delivered as EventError; the watch keeps running.
//
// The slots present at watch start are re-delivered as Bound events, so the
// watch is the single source of truth for restart recovery and no
// List-then-Watch handoff race exists.
func (s *FileSource) Watch(ctx context.Context) (<-chan Event, error) {
	initial, badFiles := s.snapshot()
	if len(badFiles) > 0 {
		return nil, fmt.Errorf("slot store unreadable at watch start (fail closed): %v", badFiles)
	}
	ch := make(chan Event, 16)
	go s.pollLoop(ctx, ch, initial)
	return ch, nil
}

func (s *FileSource) pollLoop(ctx context.Context, ch chan<- Event, initial map[string]Slot) {
	// Re-deliver the slots present at watch start as Bound events, so the
	// watch is self-sufficient (restart recovery) and a consumer that
	// rescaned via List before calling Watch still observes every subject.
	// Consumers are idempotent by contract; the state machine skips
	// deny-first for already-active subjects, so a re-delivered Bound can
	// never clobber an applied policy.
	s.emitDiffs(ctx, ch, nil, initial)
	state := initial

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			current, badFiles := s.snapshot()
			// Fail closed: a slot record that can no longer be parsed is
			// REMOVED from the view (like a deletion), so the controller
			// unloads its subject — a malformed record must never keep an
			// active subject's registry entry and nft allow rules alive.
			for name, err := range badFiles {
				select {
				case ch <- Event{Type: EventError, Err: fmt.Errorf("slot file %s unparseable: %w", name, err)}:
				case <-ctx.Done():
					return
				}
			}
			s.emitDiffs(ctx, ch, state, current)
			state = current
		}
	}
}

// emitDiffs computes per-slot lifecycle transitions between two snapshots.
// Only bound slots produce Bound/Updated; a bound slot that moves to a
// teardown phase or disappears produces Deleted. A slot observed only in a
// non-bound phase never produces an event.
func (s *FileSource) emitDiffs(ctx context.Context, ch chan<- Event, prev, cur map[string]Slot) {
	for id, slot := range cur {
		old, existed := prev[id]
		switch {
		case !existed && slot.Phase == PhaseBound:
			s.send(ctx, ch, Event{Type: EventBound, Slot: slot})
		case existed && old.Phase == PhaseBound && slot.Phase != PhaseBound:
			s.send(ctx, ch, Event{Type: EventDeleted, Slot: slot})
		case existed && old.Phase == PhaseBound && !slotEqual(old, slot):
			s.send(ctx, ch, Event{Type: EventUpdated, Slot: slot})
		}
	}
	for id, old := range prev {
		if _, ok := cur[id]; !ok && old.Phase == PhaseBound {
			s.send(ctx, ch, Event{Type: EventDeleted, Slot: old})
		}
	}
}

func (s *FileSource) send(ctx context.Context, ch chan<- Event, ev Event) {
	select {
	case ch <- ev:
	case <-ctx.Done():
	}
}

// snapshot returns the current slot view keyed by slot ID, all phases, so
// the diff can observe an in-place Bound -> Clean/Destroying transition and
// emit Deleted. Records that can no longer be parsed are EXCLUDED from the
// view and returned as badFiles: the caller (Watch) treats them as absent —
// a malformed record must fail closed (the controller unloads the subject)
// instead of keeping stale enforcement alive. A directory-level error (the
// store itself is unreadable) is fail-closed at the caller.
func (s *FileSource) snapshot() (map[string]Slot, map[string]error) {
	slots, badFiles, err := s.readDir()
	if err != nil {
		return nil, map[string]error{"<dir>": err}
	}
	out := make(map[string]Slot, len(slots))
	for _, slot := range slots {
		out[slot.ID] = slot
	}
	return out, badFiles
}

// readDir parses every *.json file in the slot store directory. Unreadable or
// unparseable records are collected per file (the healthy remainder is
// returned); only a directory-level failure is an error.
func (s *FileSource) readDir() ([]Slot, map[string]error, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, nil, fmt.Errorf("slot store dir %s: %w", s.dir, err)
	}
	var parser FileParser
	var out []Slot
	bad := make(map[string]error)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(s.dir, e.Name()))
		if err != nil {
			bad[e.Name()] = err
			continue
		}
		slot, err := parser.Parse(raw)
		if err != nil {
			bad[e.Name()] = err
			continue
		}
		out = append(out, slot)
	}
	return out, bad, nil
}

// slotEqual reports whether two snapshots of the same file are identical in
// every field the consumer acts on.
func slotEqual(a, b Slot) bool {
	return a.Phase == b.Phase &&
		a.Owner == b.Owner &&
		a.IP == b.IP &&
		a.HostNetnsPath == b.HostNetnsPath &&
		a.HostVeth == b.HostVeth &&
		a.Gateway == b.Gateway &&
		a.PrivateCIDR == b.PrivateCIDR &&
		a.DNSPath == b.DNSPath
}

// Dir returns the watched directory (diagnostics).
func (s *FileSource) Dir() string { return s.dir }
