// Copyright 2025 Alibaba Group Holding Ltd.
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

package runtime

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/tidwall/btree"

	"github.com/alibaba/opensandbox/execd/pkg/log"
)

const (
	commandCursorVersion    = 1
	commandCursorSort       = "started_at_desc_session_desc"
	commandCursorMaxEncoded = 1366
	commandCursorMaxDecoded = 1024
	commandListDefaultLimit = 100
	commandListMaxExamined  = 128
	commandCleanupLimit     = 128
)

type commandCursorFilter string

const (
	commandCursorFilterAll      commandCursorFilter = "all"
	commandCursorFilterRunning  commandCursorFilter = "running"
	commandCursorFilterTerminal commandCursorFilter = "terminal"
)

// ListCommandsRequest selects command inventory summaries.
type ListCommandsRequest struct {
	Running *bool
	Limit   int
	Cursor  string
}

// CommandSummary is a snapshot of a command inventory entry.
type CommandSummary struct {
	Session    string
	Running    bool
	Background bool
	StartedAt  time.Time
	FinishedAt *time.Time
	ExitCode   *int
	Error      string
}

// ListCommandsResponse contains a command inventory page.
type ListCommandsResponse struct {
	Commands   []CommandSummary
	Pagination CommandPagination
}

// CommandPagination describes command inventory page traversal.
type CommandPagination struct {
	Limit      int
	NextCursor *string
}

type commandCursor struct {
	Version  int                 `json:"version"`
	Process  string              `json:"process"`
	Filter   commandCursorFilter `json:"filter"`
	Sort     string              `json:"sort"`
	Frontier *commandCursorKey   `json:"frontier,omitempty"`
}

type commandCursorKey struct {
	StartedAtUnixNano *int64 `json:"started_at_unix_nano"`
	Session           string `json:"session"`
}

// inventoryEntry is an immutable snapshot of a command's inventory state.
type inventoryEntry struct {
	session          string
	running          bool
	background       bool
	startedAt        time.Time
	finishedAt       *time.Time
	exitCode         *int
	errMsg           string
	terminalSequence uint64
	indexSession     string
	indexStartedAt   int64
}

// commandTerminalSnapshot is a value copy of the legacy command state at exit.
// It may be consumed only after Controller.mu has been released.
type commandTerminalSnapshot struct {
	session    string
	startedAt  time.Time
	finishedAt time.Time
	exitCode   int
	errMsg     string
	background bool
}

func inventoryEntryStartedLess(a, b *inventoryEntry) bool {
	if a.startedAt.UnixNano() != b.startedAt.UnixNano() {
		return a.startedAt.UnixNano() < b.startedAt.UnixNano()
	}
	return a.session < b.session
}

func inventoryEntryTerminalFinishedLess(a, b *inventoryEntry) bool {
	if a.finishedAt.UnixNano() != b.finishedAt.UnixNano() {
		return a.finishedAt.UnixNano() < b.finishedAt.UnixNano()
	}
	return a.terminalSequence < b.terminalSequence
}

func (c *Controller) registerRunningCommand(summary inventoryEntry) error {
	if !summary.running {
		return fmt.Errorf("command inventory invariant: running entry is not running")
	}

	c.inventoryMu.Lock()
	defer c.inventoryMu.Unlock()

	if _, ok := c.bySession[summary.session]; ok {
		return fmt.Errorf("command inventory invariant: session %q already exists", summary.session)
	}

	entry := &inventoryEntry{
		session:        summary.session,
		running:        true,
		background:     summary.background,
		startedAt:      summary.startedAt,
		indexSession:   summary.session,
		indexStartedAt: summary.startedAt.UnixNano(),
	}
	if err := ensureIndexAbsent(c.runningByStartedAt, entry, "running start"); err != nil {
		return err
	}
	if previous, replaced := c.runningByStartedAt.Set(entry); replaced {
		c.runningByStartedAt.Set(previous)
		return fmt.Errorf("command inventory invariant: running start index replaced %q", previous.session)
	}
	c.bySession[entry.session] = entry
	return nil
}

func (c *Controller) registerRunningCommandSummary(summary inventoryEntry) {
	if err := c.registerRunningCommand(summary); err != nil {
		log.Error("command inventory registration failed: %v", err)
	}
}

func (c *Controller) registerCommandKernelRunning(session string, kernel *commandKernel) {
	c.registerRunningCommandSummary(inventoryEntry{
		session:    session,
		running:    true,
		background: kernel.isBackground,
		startedAt:  kernel.startedAt,
	})
}

// transitionCommandTerminalSnapshot records a terminal inventory summary after
// legacy command bookkeeping has released Controller.mu.
func (c *Controller) transitionCommandTerminalSnapshot(snapshot commandTerminalSnapshot) {
	if snapshot.session == "" {
		return
	}
	finishedAt := snapshot.finishedAt
	if err := c.transitionCommandTerminal(inventoryEntry{
		session:    snapshot.session,
		background: snapshot.background,
		startedAt:  snapshot.startedAt,
		finishedAt: &finishedAt,
		exitCode:   &snapshot.exitCode,
		errMsg:     snapshot.errMsg,
	}); err != nil {
		log.Error("command inventory terminal transition failed: %v", err)
	}
}

func (c *Controller) transitionCommandTerminal(summary inventoryEntry) error {
	c.inventoryMu.Lock()
	defer c.inventoryMu.Unlock()

	current, ok := c.bySession[summary.session]
	if !ok {
		return fmt.Errorf("command inventory invariant: session %q is absent", summary.session)
	}
	if !current.running {
		return nil
	}
	if summary.running || summary.finishedAt == nil {
		return fmt.Errorf("command inventory invariant: terminal entry is invalid")
	}
	if summary.background != current.background || !summary.startedAt.Equal(current.startedAt) {
		return fmt.Errorf("command inventory invariant: terminal entry does not match running session %q", current.session)
	}
	if err := ensureIndexIdentity(c.runningByStartedAt, current, "running start"); err != nil {
		return err
	}

	finishedAt := *summary.finishedAt
	entry := &inventoryEntry{
		session:          current.session,
		background:       current.background,
		startedAt:        current.startedAt,
		finishedAt:       &finishedAt,
		exitCode:         cloneExitCode(summary.exitCode),
		errMsg:           summary.errMsg,
		terminalSequence: c.terminalSequence + 1,
		indexSession:     current.session,
		indexStartedAt:   current.startedAt.UnixNano(),
	}
	if err := ensureIndexAbsent(c.terminalByStartedAt, entry, "terminal start"); err != nil {
		return err
	}
	if err := ensureIndexAbsent(c.terminalByFinishedAt, entry, "terminal finish"); err != nil {
		return err
	}

	if err := deleteInventoryEntry(c.runningByStartedAt, current, "running start"); err != nil {
		return err
	}
	if previous, replaced := c.terminalByStartedAt.Set(entry); replaced {
		c.terminalByStartedAt.Set(previous)
		c.runningByStartedAt.Set(current)
		return fmt.Errorf("command inventory invariant: terminal start index replaced %q", previous.session)
	}
	if previous, replaced := c.terminalByFinishedAt.Set(entry); replaced {
		c.terminalByFinishedAt.Set(previous)
		if _, deleted := c.terminalByStartedAt.Delete(entry); !deleted {
			return fmt.Errorf("command inventory invariant: terminal start rollback failed for %q", entry.session)
		}
		c.runningByStartedAt.Set(current)
		return fmt.Errorf("command inventory invariant: terminal finish index replaced %q", previous.session)
	}
	c.terminalSequence = entry.terminalSequence
	c.bySession[entry.session] = entry

	if c.commandInventoryConfig.RecoveryTTL <= 0 || c.commandInventoryConfig.MaxTerminal <= 0 {
		if err := c.deleteTerminalLocked(entry); err != nil {
			return err
		}
	} else if err := c.evictTerminalOverCapacityLocked(); err != nil {
		return err
	}
	return nil
}

func (c *Controller) evictTerminalOverCapacityLocked() error {
	maxTerminal := c.commandInventoryConfig.MaxTerminal
	for c.terminalByFinishedAt.Len() > maxTerminal {
		entry, ok := c.terminalByFinishedAt.Min()
		if !ok {
			return fmt.Errorf("command inventory invariant: terminal finish index is empty above capacity")
		}
		if err := c.deleteTerminalLocked(entry); err != nil {
			return err
		}
	}
	return nil
}

func (c *Controller) deleteTerminalLocked(entry *inventoryEntry) error {
	if c.bySession[entry.session] != entry {
		return fmt.Errorf("command inventory invariant: session %q identity changed", entry.session)
	}
	if entry.running {
		return fmt.Errorf("command inventory invariant: session %q is running", entry.session)
	}
	if err := ensureIndexIdentity(c.terminalByStartedAt, entry, "terminal start"); err != nil {
		return err
	}
	if err := ensureIndexIdentity(c.terminalByFinishedAt, entry, "terminal finish"); err != nil {
		return err
	}
	if err := deleteInventoryEntry(c.terminalByStartedAt, entry, "terminal start"); err != nil {
		return err
	}
	if err := deleteInventoryEntry(c.terminalByFinishedAt, entry, "terminal finish"); err != nil {
		if previous, replaced := c.terminalByStartedAt.Set(entry); replaced || previous != nil {
			return fmt.Errorf("command inventory invariant: terminal start rollback failed for %q", entry.session)
		}
		return err
	}
	delete(c.bySession, entry.session)
	return nil
}

func ensureIndexAbsent(index *btree.BTreeG[*inventoryEntry], entry *inventoryEntry, name string) error {
	if existing, ok := index.Get(entry); ok {
		return fmt.Errorf("command inventory invariant: %s index already contains %q", name, existing.session)
	}
	return nil
}

func ensureIndexIdentity(index *btree.BTreeG[*inventoryEntry], entry *inventoryEntry, name string) error {
	stored, ok := index.Get(entry)
	if !ok {
		return fmt.Errorf("command inventory invariant: %s index is missing %q", name, entry.session)
	}
	if stored != entry {
		return fmt.Errorf("command inventory invariant: %s index identity changed for %q", name, entry.session)
	}
	return nil
}

func deleteInventoryEntry(index *btree.BTreeG[*inventoryEntry], entry *inventoryEntry, name string) error {
	deleted, ok := index.Delete(entry)
	if !ok {
		return fmt.Errorf("command inventory invariant: %s index is missing %q", name, entry.session)
	}
	if deleted != entry {
		return fmt.Errorf("command inventory invariant: %s index identity changed for %q", name, entry.session)
	}
	return nil
}

func cloneExitCode(exitCode *int) *int {
	if exitCode == nil {
		return nil
	}
	cloned := *exitCode
	return &cloned
}

// ListCommands returns a globally ordered page of inventory summaries. Entries
// sort by started time and session in descending order.
func (c *Controller) ListCommands(request ListCommandsRequest) (ListCommandsResponse, error) {
	filter := commandFilterForRequest(request.Running)
	cursor, err := c.parseCommandCursor(request.Cursor, filter)
	if err != nil {
		return ListCommandsResponse{}, err
	}

	limit := request.Limit
	if limit <= 0 {
		limit = commandListDefaultLimit
	}
	if limit > commandListMaxExamined {
		limit = commandListMaxExamined
	}

	cleanupOwner := c.cleanupOwner.CompareAndSwap(false, true)
	if cleanupOwner {
		defer c.cleanupOwner.Store(false)
	}
	c.inventoryMu.Lock()
	defer c.inventoryMu.Unlock()
	now := time.Now()
	if cleanupOwner {
		if _, err := c.cleanupExpiredLocked(now); err != nil {
			return ListCommandsResponse{}, err
		}
	}
	return c.listCommandsLocked(filter, limit, cursor.Frontier, now)
}

func commandFilterForRequest(running *bool) commandCursorFilter {
	if running == nil {
		return commandCursorFilterAll
	}
	if *running {
		return commandCursorFilterRunning
	}
	return commandCursorFilterTerminal
}

func (c *Controller) parseCommandCursor(raw string, filter commandCursorFilter) (commandCursor, error) {
	if raw == "" {
		return commandCursor{Filter: filter}, nil
	}
	if len(raw) > commandCursorMaxEncoded {
		return commandCursor{}, invalidCommandCursor()
	}
	for i := range len(raw) {
		if !isRawURLBase64Character(raw[i]) {
			return commandCursor{}, invalidCommandCursor()
		}
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(raw)
	if err != nil || len(decoded) > commandCursorMaxDecoded {
		return commandCursor{}, invalidCommandCursor()
	}

	decoder := json.NewDecoder(bytes.NewReader(decoded))
	decoder.DisallowUnknownFields()
	var cursor commandCursor
	if err := decoder.Decode(&cursor); err != nil {
		return commandCursor{}, invalidCommandCursor()
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return commandCursor{}, invalidCommandCursor()
	}
	if cursor.Version != commandCursorVersion || cursor.Process != c.inventoryProcessID ||
		cursor.Filter != filter || !validCommandCursorFilter(cursor.Filter) || cursor.Sort != commandCursorSort {
		return commandCursor{}, invalidCommandCursor()
	}
	if cursor.Frontier == nil || cursor.Frontier.StartedAtUnixNano == nil || cursor.Frontier.Session == "" {
		return commandCursor{}, invalidCommandCursor()
	}
	return cursor, nil
}

func isRawURLBase64Character(character byte) bool {
	return character >= 'A' && character <= 'Z' || character >= 'a' && character <= 'z' ||
		character >= '0' && character <= '9' || character == '-' || character == '_'
}

func validCommandCursorFilter(filter commandCursorFilter) bool {
	return filter == commandCursorFilterAll || filter == commandCursorFilterRunning || filter == commandCursorFilterTerminal
}

func invalidCommandCursor() error {
	return fmt.Errorf("%w", ErrInvalidCommandCursor)
}

func (c *Controller) encodeCommandCursor(filter commandCursorFilter, frontier *commandCursorKey) (string, error) {
	if !validCommandCursorFilter(filter) || frontier == nil || frontier.StartedAtUnixNano == nil || frontier.Session == "" {
		return "", invalidCommandCursor()
	}
	payload, err := json.Marshal(commandCursor{
		Version:  commandCursorVersion,
		Process:  c.inventoryProcessID,
		Filter:   filter,
		Sort:     commandCursorSort,
		Frontier: frontier,
	})
	if err != nil || len(payload) > commandCursorMaxDecoded {
		return "", invalidCommandCursor()
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	if len(encoded) > commandCursorMaxEncoded {
		return "", invalidCommandCursor()
	}
	return encoded, nil
}

func (c *Controller) listCommandsLocked(filter commandCursorFilter, limit int, frontier *commandCursorKey, now time.Time) (ListCommandsResponse, error) {
	response := ListCommandsResponse{
		Commands:   make([]CommandSummary, 0, limit),
		Pagination: CommandPagination{Limit: limit},
	}
	var running, terminal *commandInventoryDescendingIterator
	if filter != commandCursorFilterTerminal {
		running = newCommandInventoryDescendingIterator(c.runningByStartedAt, frontier, false)
		defer running.release()
	}
	if filter != commandCursorFilterRunning {
		terminal = newCommandInventoryDescendingIterator(c.terminalByStartedAt, frontier, true)
		defer terminal.release()
	}

	budget := commandInventoryPhysicalBudget{limit: commandListMaxExamined}
	load := func(iterator *commandInventoryDescendingIterator) commandInventoryLoadResult {
		if iterator == nil {
			return commandInventoryLoadExhausted
		}
		return iterator.load(&budget)
	}
	runningLoad := load(running)
	terminalLoad := load(terminal)
	if runningLoad == commandInventoryLoadBudget || terminalLoad == commandInventoryLoadBudget {
		return response, nil
	}

	var consumed *commandCursorKey
	budgetExhausted := false
	for {
		winner, winnerIterator := commandInventoryWinner(running, terminal)
		if winner == nil {
			break
		}
		if err := c.validateListInventoryEntryLocked(winner, winnerIterator.terminal); err != nil {
			return ListCommandsResponse{}, err
		}

		key := commandCursorKeyForEntry(winner)
		if frontier != nil && !commandCursorKeyLess(&key, frontier) {
			return ListCommandsResponse{}, fmt.Errorf("command inventory invariant: cursor did not strictly advance")
		}
		if winnerIterator.terminal && c.commandTerminalExpired(winner, now) {
			if err := c.deleteTerminalLocked(winner); err != nil {
				return ListCommandsResponse{}, err
			}
			consumed = &key
			frontier = consumed
			winnerIterator.clearAfterDelete(frontier)
		} else {
			if len(response.Commands) == limit {
				break
			}
			response.Commands = append(response.Commands, commandSummaryFromInventoryEntry(winner))
			consumed = &key
			frontier = consumed
			winnerIterator.advance()
		}

		if load(winnerIterator) == commandInventoryLoadBudget {
			budgetExhausted = true
			break
		}
	}

	if consumed != nil {
		remaining, _ := commandInventoryWinner(running, terminal)
		knownExhausted := !budgetExhausted && remaining == nil
		if !knownExhausted {
			next, err := c.encodeCommandCursor(filter, consumed)
			if err != nil {
				return ListCommandsResponse{}, err
			}
			response.Pagination.NextCursor = &next
		}
	} else if remaining, _ := commandInventoryWinner(running, terminal); remaining != nil {
		return ListCommandsResponse{}, fmt.Errorf("command inventory invariant: no progress while command inventory data remains")
	}
	return response, nil
}

type commandInventoryDescendingIterator struct {
	index     *btree.BTreeG[*inventoryEntry]
	iterator  btree.IterG[*inventoryEntry]
	frontier  *commandCursorKey
	entry     *inventoryEntry
	loaded    bool
	exhausted bool
	terminal  bool
	started   bool
}

type commandInventoryLoadResult uint8

const (
	commandInventoryLoadReady commandInventoryLoadResult = iota
	commandInventoryLoadExhausted
	commandInventoryLoadBudget
)

type commandInventoryPhysicalBudget struct {
	limit        int
	materialized int
}

func (budget *commandInventoryPhysicalBudget) item(iterator *btree.IterG[*inventoryEntry]) (*inventoryEntry, bool) {
	if budget.materialized >= budget.limit {
		return nil, false
	}
	budget.materialized++
	return iterator.Item(), true
}

func newCommandInventoryDescendingIterator(index *btree.BTreeG[*inventoryEntry], frontier *commandCursorKey, terminal bool) *commandInventoryDescendingIterator {
	return &commandInventoryDescendingIterator{index: index, iterator: index.Iter(), frontier: frontier, terminal: terminal}
}

func (iterator *commandInventoryDescendingIterator) release() {
	iterator.iterator.Release()
}

func (iterator *commandInventoryDescendingIterator) load(budget *commandInventoryPhysicalBudget) commandInventoryLoadResult {
	if iterator.loaded || iterator.exhausted {
		if iterator.loaded {
			return commandInventoryLoadReady
		}
		return commandInventoryLoadExhausted
	}
	if !iterator.started {
		iterator.started = true
		if iterator.frontier == nil {
			if !iterator.iterator.Last() {
				iterator.exhausted = true
				return commandInventoryLoadExhausted
			}
		} else {
			key := inventoryEntry{session: iterator.frontier.Session, startedAt: time.Unix(0, *iterator.frontier.StartedAtUnixNano)}
			if iterator.iterator.Seek(&key) {
				_, ok := budget.item(&iterator.iterator)
				if !ok {
					return commandInventoryLoadBudget
				}
				if !iterator.iterator.Prev() {
					iterator.exhausted = true
					return commandInventoryLoadExhausted
				}
			} else if !iterator.iterator.Last() {
				iterator.exhausted = true
				return commandInventoryLoadExhausted
			}
		}
	} else if !iterator.iterator.Prev() {
		iterator.exhausted = true
		return commandInventoryLoadExhausted
	}
	entry, ok := budget.item(&iterator.iterator)
	if !ok {
		return commandInventoryLoadBudget
	}
	iterator.entry = entry
	iterator.loaded = true
	return commandInventoryLoadReady
}

func (iterator *commandInventoryDescendingIterator) advance() {
	iterator.loaded = false
	iterator.entry = nil
}

func (iterator *commandInventoryDescendingIterator) clearAfterDelete(frontier *commandCursorKey) {
	iterator.iterator.Release()
	iterator.iterator = iterator.index.Iter()
	iterator.loaded = false
	iterator.entry = nil
	iterator.frontier = frontier
	iterator.started = false
}

func commandInventoryWinner(running, terminal *commandInventoryDescendingIterator) (*inventoryEntry, *commandInventoryDescendingIterator) {
	if running == nil || !running.loaded {
		if terminal == nil || !terminal.loaded {
			return nil, nil
		}
		return terminal.entry, terminal
	}
	if terminal == nil || !terminal.loaded {
		return running.entry, running
	}
	if inventoryEntryStartedLess(running.entry, terminal.entry) {
		return terminal.entry, terminal
	}
	return running.entry, running
}

func commandCursorKeyForEntry(entry *inventoryEntry) commandCursorKey {
	startedAtUnixNano := entry.startedAt.UnixNano()
	return commandCursorKey{StartedAtUnixNano: &startedAtUnixNano, Session: entry.session}
}

func commandCursorKeyLess(a, b *commandCursorKey) bool {
	if *a.StartedAtUnixNano != *b.StartedAtUnixNano {
		return *a.StartedAtUnixNano < *b.StartedAtUnixNano
	}
	return a.Session < b.Session
}

func (c *Controller) validateListInventoryEntryLocked(entry *inventoryEntry, terminal bool) error {
	if entry == nil || c.bySession[entry.session] != entry {
		return fmt.Errorf("command inventory invariant: list entry identity changed")
	}
	if entry.indexSession != entry.session || entry.indexStartedAt != entry.startedAt.UnixNano() {
		return fmt.Errorf("command inventory invariant: list entry key changed")
	}
	if terminal {
		if entry.running || entry.finishedAt == nil {
			return fmt.Errorf("command inventory invariant: terminal list entry is invalid")
		}
		if err := ensureIndexIdentity(c.terminalByStartedAt, entry, "terminal start"); err != nil {
			return err
		}
		return ensureIndexIdentity(c.terminalByFinishedAt, entry, "terminal finish")
	}
	if !entry.running {
		return fmt.Errorf("command inventory invariant: running list entry is invalid")
	}
	return ensureIndexIdentity(c.runningByStartedAt, entry, "running start")
}

func commandSummaryFromInventoryEntry(entry *inventoryEntry) CommandSummary {
	return CommandSummary{
		Session: entry.session, Running: entry.running, Background: entry.background,
		StartedAt: entry.startedAt, FinishedAt: cloneTime(entry.finishedAt),
		ExitCode: cloneExitCode(entry.exitCode), Error: entry.errMsg,
	}
}

func cloneTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func (c *Controller) commandTerminalExpired(entry *inventoryEntry, now time.Time) bool {
	return c.commandInventoryConfig.RecoveryTTL > 0 && entry.finishedAt != nil &&
		!now.Before(entry.finishedAt.Add(c.commandInventoryConfig.RecoveryTTL))
}

func (c *Controller) cleanupExpiredLocked(now time.Time) (int, error) {
	if c.commandInventoryConfig.RecoveryTTL <= 0 {
		return 0, nil
	}
	deleted := 0
	materialized := 0
	for materialized < commandCleanupLimit {
		entry, ok := c.terminalByFinishedAt.Min()
		if !ok {
			break
		}
		materialized++
		if !c.commandTerminalExpired(entry, now) {
			break
		}
		if err := c.deleteTerminalLocked(entry); err != nil {
			return deleted, err
		}
		deleted++
	}
	return deleted, nil
}
