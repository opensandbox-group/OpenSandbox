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
	"encoding/base64"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCommandInventoryList(t *testing.T) {
	t.Run("global pagination", testCommandInventoryListGlobalPagination)
	t.Run("limit does not skip opposing lookahead", testCommandInventoryListLimitDoesNotSkipOpposingLookahead)
	t.Run("exact final page omits cursor", testCommandInventoryListExactFinalPageOmitsCursor)
	t.Run("filters and cursor binding", testCommandInventoryListFiltersAndCursorBinding)
	t.Run("deleted anchor is an exclusive seek", testCommandInventoryListDeletedAnchor)
	t.Run("running migration does not duplicate past frontier", testCommandInventoryListRunningMigrationDoesNotDuplicate)
	t.Run("same timestamp sorts session descending", testCommandInventoryListSameTimestampSessions)
	t.Run("expired discard advances cursor", testCommandInventoryListExpiredDiscardAdvancesCursor)
	t.Run("cleanup removed cursor anchor", testCommandInventoryListCleanupRemovedAnchor)
	t.Run("summary pointers are isolated", testCommandInventoryListSummaryIsolation)
	t.Run("invariant failures", testCommandInventoryListInvariantFailures)
	t.Run("physical budget is bounded", testCommandInventoryListPhysicalBudget)
}

func testCommandInventoryListGlobalPagination(t *testing.T) {
	c := NewController("", "", WithCommandInventory(CommandInventoryConfig{
		RecoveryTTL: time.Hour,
		MaxTerminal: 100,
	}))
	base := time.Now().UTC().Add(-time.Minute)
	for _, command := range []struct {
		session string
		started int
		running bool
	}{
		{session: "running-z", started: 4, running: true},
		{session: "terminal-y", started: 3},
		{session: "running-x", started: 2, running: true},
		{session: "terminal-w", started: 1},
	} {
		startedAt := base.Add(time.Duration(command.started) * time.Nanosecond)
		mustRegisterRunning(t, c, inventoryEntry{
			session: command.session, running: true, startedAt: startedAt,
		})
		if !command.running {
			finishedAt := startedAt.Add(time.Second)
			mustTransitionTerminal(t, c, inventoryEntry{
				session: command.session, startedAt: startedAt, finishedAt: &finishedAt,
			})
		}
	}

	var got []string
	seen := make(map[string]struct{})
	request := ListCommandsRequest{Limit: 1}
	var previous *commandCursorKey
	for {
		response, err := c.ListCommands(request)
		require.NoError(t, err)
		require.Equal(t, 1, response.Pagination.Limit)
		for _, command := range response.Commands {
			_, duplicate := seen[command.Session]
			require.Falsef(t, duplicate, "duplicate command %q across pages", command.Session)
			seen[command.Session] = struct{}{}
			got = append(got, command.Session)
		}
		if response.Pagination.NextCursor == nil {
			break
		}
		cursor, err := c.parseCommandCursor(*response.Pagination.NextCursor, commandCursorFilterAll)
		require.NoError(t, err)
		if previous != nil {
			require.True(t, commandCursorKeyLess(cursor.Frontier, previous), "cursor must strictly progress")
		}
		previous = cursor.Frontier
		request.Cursor = *response.Pagination.NextCursor
	}
	require.Equal(t, []string{"running-z", "terminal-y", "running-x", "terminal-w"}, got)
}

func testCommandInventoryListLimitDoesNotSkipOpposingLookahead(t *testing.T) {
	c := NewController("", "", WithCommandInventory(CommandInventoryConfig{
		RecoveryTTL: time.Hour,
		MaxTerminal: 10,
	}))
	base := time.Now().UTC().Add(-time.Minute)
	for _, command := range []struct {
		session string
		started int
		running bool
	}{
		{session: "running-newest", started: 2, running: true},
		{session: "terminal-next", started: 1},
	} {
		startedAt := base.Add(time.Duration(command.started) * time.Nanosecond)
		mustRegisterRunning(t, c, inventoryEntry{session: command.session, running: true, startedAt: startedAt})
		if !command.running {
			finishedAt := startedAt.Add(time.Second)
			mustTransitionTerminal(t, c, inventoryEntry{session: command.session, startedAt: startedAt, finishedAt: &finishedAt})
		}
	}

	first, err := c.ListCommands(ListCommandsRequest{Limit: 1})
	require.NoError(t, err)
	require.Equal(t, []string{"running-newest"}, commandSummarySessions(first.Commands))
	require.NotNil(t, first.Pagination.NextCursor)

	second, err := c.ListCommands(ListCommandsRequest{Limit: 1, Cursor: *first.Pagination.NextCursor})
	require.NoError(t, err)
	require.Equal(t, []string{"terminal-next"}, commandSummarySessions(second.Commands))
}

func testCommandInventoryListExactFinalPageOmitsCursor(t *testing.T) {
	base := time.Now().UTC().Add(-time.Minute)
	newController := func() *Controller {
		return NewController("", "", WithCommandInventory(CommandInventoryConfig{
			RecoveryTTL: time.Hour,
			MaxTerminal: 10,
		}))
	}

	t.Run("running only", func(t *testing.T) {
		c := newController()
		mustRegisterRunning(t, c, inventoryEntry{session: "newer", running: true, startedAt: base.Add(time.Nanosecond)})
		mustRegisterRunning(t, c, inventoryEntry{session: "older", running: true, startedAt: base})
		running := true

		response, err := c.ListCommands(ListCommandsRequest{Running: &running, Limit: 2})
		require.NoError(t, err)
		require.Equal(t, []string{"newer", "older"}, commandSummarySessions(response.Commands))
		require.Nil(t, response.Pagination.NextCursor)
	})

	t.Run("terminal only", func(t *testing.T) {
		c := newController()
		registerTerminalInventory(t, c, "newer", base.Add(time.Nanosecond), base.Add(time.Second))
		registerTerminalInventory(t, c, "older", base, base.Add(time.Second))
		terminal := false

		response, err := c.ListCommands(ListCommandsRequest{Running: &terminal, Limit: 2})
		require.NoError(t, err)
		require.Equal(t, []string{"newer", "older"}, commandSummarySessions(response.Commands))
		require.Nil(t, response.Pagination.NextCursor)
	})

	t.Run("mixed", func(t *testing.T) {
		c := newController()
		mustRegisterRunning(t, c, inventoryEntry{session: "running", running: true, startedAt: base.Add(time.Nanosecond)})
		registerTerminalInventory(t, c, "terminal", base, base.Add(time.Second))

		response, err := c.ListCommands(ListCommandsRequest{Limit: 2})
		require.NoError(t, err)
		require.Equal(t, []string{"running", "terminal"}, commandSummarySessions(response.Commands))
		require.Nil(t, response.Pagination.NextCursor)
	})

	t.Run("filtered", func(t *testing.T) {
		c := newController()
		mustRegisterRunning(t, c, inventoryEntry{session: "running-newer", running: true, startedAt: base.Add(3 * time.Nanosecond)})
		registerTerminalInventory(t, c, "terminal-newer", base.Add(2*time.Nanosecond), base.Add(time.Second))
		mustRegisterRunning(t, c, inventoryEntry{session: "running-older", running: true, startedAt: base.Add(time.Nanosecond)})
		registerTerminalInventory(t, c, "terminal-older", base, base.Add(time.Second))
		running := true

		response, err := c.ListCommands(ListCommandsRequest{Running: &running, Limit: 2})
		require.NoError(t, err)
		require.Equal(t, []string{"running-newer", "running-older"}, commandSummarySessions(response.Commands))
		require.Nil(t, response.Pagination.NextCursor)
	})
}

func testCommandInventoryListFiltersAndCursorBinding(t *testing.T) {
	c := NewController("", "", WithCommandInventory(CommandInventoryConfig{RecoveryTTL: time.Hour, MaxTerminal: 10}))
	startedAt := time.Now().UTC().Add(-time.Minute)
	mustRegisterRunning(t, c, inventoryEntry{session: "running", running: true, startedAt: startedAt.Add(time.Nanosecond)})
	mustRegisterRunning(t, c, inventoryEntry{session: "terminal", running: true, startedAt: startedAt})
	mustRegisterRunning(t, c, inventoryEntry{session: "running-older", running: true, startedAt: startedAt.Add(-time.Nanosecond)})
	finishedAt := startedAt.Add(time.Second)
	mustTransitionTerminal(t, c, inventoryEntry{session: "terminal", startedAt: startedAt, finishedAt: &finishedAt})

	running := true
	response, err := c.ListCommands(ListCommandsRequest{Running: &running, Limit: 1})
	require.NoError(t, err)
	require.Equal(t, []string{"running"}, commandSummarySessions(response.Commands))
	require.NotNil(t, response.Pagination.NextCursor)

	terminal := false
	_, err = c.ListCommands(ListCommandsRequest{Running: &terminal, Limit: 1, Cursor: *response.Pagination.NextCursor})
	require.ErrorIs(t, err, ErrInvalidCommandCursor)

	other := NewController("", "")
	_, err = other.ListCommands(ListCommandsRequest{Running: &running, Limit: 1, Cursor: *response.Pagination.NextCursor})
	require.ErrorIs(t, err, ErrInvalidCommandCursor)
}

func testCommandInventoryListDeletedAnchor(t *testing.T) {
	c := NewController("", "", WithCommandInventory(CommandInventoryConfig{RecoveryTTL: time.Hour, MaxTerminal: 10}))
	base := time.Now().UTC().Add(-time.Minute)
	for i, session := range []string{"newer", "older"} {
		startedAt := base.Add(time.Duration(2-i) * time.Nanosecond)
		mustRegisterRunning(t, c, inventoryEntry{session: session, running: true, startedAt: startedAt})
	}
	first, err := c.ListCommands(ListCommandsRequest{Limit: 1})
	require.NoError(t, err)
	require.Equal(t, []string{"newer"}, commandSummarySessions(first.Commands))
	c.inventoryMu.Lock()
	entry := c.bySession["newer"]
	require.NoError(t, deleteInventoryEntry(c.runningByStartedAt, entry, "running start"))
	delete(c.bySession, entry.session)
	c.inventoryMu.Unlock()
	second, err := c.ListCommands(ListCommandsRequest{Limit: 1, Cursor: *first.Pagination.NextCursor})
	require.NoError(t, err)
	require.Equal(t, []string{"older"}, commandSummarySessions(second.Commands))
}

func testCommandInventoryListRunningMigrationDoesNotDuplicate(t *testing.T) {
	c := NewController("", "", WithCommandInventory(CommandInventoryConfig{RecoveryTTL: time.Hour, MaxTerminal: 10}))
	startedAt := time.Now().UTC().Add(-time.Minute)
	mustRegisterRunning(t, c, inventoryEntry{session: "session", running: true, startedAt: startedAt})
	mustRegisterRunning(t, c, inventoryEntry{session: "older", running: true, startedAt: startedAt.Add(-time.Nanosecond)})
	first, err := c.ListCommands(ListCommandsRequest{Limit: 1})
	require.NoError(t, err)
	require.Equal(t, []string{"session"}, commandSummarySessions(first.Commands))
	finishedAt := startedAt.Add(time.Second)
	mustTransitionTerminal(t, c, inventoryEntry{session: "session", startedAt: startedAt, finishedAt: &finishedAt})
	second, err := c.ListCommands(ListCommandsRequest{Limit: 1, Cursor: *first.Pagination.NextCursor})
	require.NoError(t, err)
	require.Equal(t, []string{"older"}, commandSummarySessions(second.Commands))
}

func testCommandInventoryListSameTimestampSessions(t *testing.T) {
	c := NewController("", "")
	startedAt := time.Now().UTC().Add(-time.Minute)
	for _, session := range []string{"a", "c", "b"} {
		mustRegisterRunning(t, c, inventoryEntry{session: session, running: true, startedAt: startedAt})
	}
	response, err := c.ListCommands(ListCommandsRequest{Limit: 3})
	require.NoError(t, err)
	require.Equal(t, []string{"c", "b", "a"}, commandSummarySessions(response.Commands))
}

func testCommandInventoryListExpiredDiscardAdvancesCursor(t *testing.T) {
	c := NewController("", "", WithCommandInventory(CommandInventoryConfig{RecoveryTTL: time.Hour, MaxTerminal: 300}))
	base := time.Now().UTC().Add(-2 * time.Hour)
	for i := range 257 {
		registerTerminalInventory(t, c, fmt.Sprintf("expired-%03d", i), base.Add(time.Duration(i)*time.Nanosecond), base)
	}
	response, err := c.ListCommands(ListCommandsRequest{Limit: 1})
	require.NoError(t, err)
	require.Empty(t, response.Commands)
	require.NotNil(t, response.Pagination.NextCursor, "expired winner consumption must preserve continuation")
	following, err := c.ListCommands(ListCommandsRequest{Limit: 1, Cursor: *response.Pagination.NextCursor})
	require.NoError(t, err)
	require.Empty(t, following.Commands)
	require.Nil(t, following.Pagination.NextCursor)
}

func testCommandInventoryListCleanupRemovedAnchor(t *testing.T) {
	c := NewController("", "", WithCommandInventory(CommandInventoryConfig{RecoveryTTL: time.Hour, MaxTerminal: 10}))
	base := time.Now().UTC().Add(-time.Minute)
	registerTerminalInventory(t, c, "anchor", base.Add(time.Nanosecond), base)
	registerTerminalInventory(t, c, "older", base, base)
	first, err := c.ListCommands(ListCommandsRequest{Limit: 1})
	require.NoError(t, err)
	require.Equal(t, []string{"anchor"}, commandSummarySessions(first.Commands))
	c.inventoryMu.Lock()
	_, err = c.cleanupExpiredLocked(base.Add(time.Hour))
	c.inventoryMu.Unlock()
	require.NoError(t, err)
	next, err := c.ListCommands(ListCommandsRequest{Limit: 1, Cursor: *first.Pagination.NextCursor})
	require.NoError(t, err)
	require.Empty(t, next.Commands)
}

func testCommandInventoryListSummaryIsolation(t *testing.T) {
	c := NewController("", "")
	startedAt := time.Now().UTC().Add(-time.Minute)
	finishedAt := startedAt.Add(time.Second)
	exitCode := 17
	mustRegisterRunning(t, c, inventoryEntry{session: "terminal", running: true, startedAt: startedAt})
	mustTransitionTerminal(t, c, inventoryEntry{session: "terminal", startedAt: startedAt, finishedAt: &finishedAt, exitCode: &exitCode})
	first, err := c.ListCommands(ListCommandsRequest{Limit: 1})
	require.NoError(t, err)
	*first.Commands[0].FinishedAt = time.Time{}
	*first.Commands[0].ExitCode = 99
	c.inventoryMu.Lock()
	entry := c.bySession["terminal"]
	require.Equal(t, finishedAt, *entry.finishedAt)
	require.Equal(t, 17, *entry.exitCode)
	assertIndexIdentity(t, c.terminalByFinishedAt, entry, "terminal finish")
	c.inventoryMu.Unlock()
	second, err := c.ListCommands(ListCommandsRequest{Limit: 1})
	require.NoError(t, err)
	require.Equal(t, finishedAt, *second.Commands[0].FinishedAt)
	require.Equal(t, 17, *second.Commands[0].ExitCode)
}

func testCommandInventoryListInvariantFailures(t *testing.T) {
	newRunning := func(t *testing.T) (*Controller, *inventoryEntry) {
		t.Helper()
		c := NewController("", "")
		mustRegisterRunning(t, c, inventoryEntry{session: "entry", running: true, startedAt: time.Now().UTC().Add(-time.Minute)})
		return c, c.bySession["entry"]
	}
	t.Run("map identity", func(t *testing.T) {
		c, entry := newRunning(t)
		c.inventoryMu.Lock()
		c.bySession[entry.session] = &inventoryEntry{session: entry.session, running: true, startedAt: entry.startedAt}
		c.inventoryMu.Unlock()
		_, err := c.ListCommands(ListCommandsRequest{Limit: 1})
		require.ErrorContains(t, err, "invariant")
	})
	t.Run("key mismatch", func(t *testing.T) {
		c, entry := newRunning(t)
		c.inventoryMu.Lock()
		entry.startedAt = entry.startedAt.Add(time.Nanosecond)
		c.inventoryMu.Unlock()
		_, err := c.ListCommands(ListCommandsRequest{Limit: 1})
		require.ErrorContains(t, err, "invariant")
	})
	t.Run("state mismatch", func(t *testing.T) {
		c, entry := newRunning(t)
		c.inventoryMu.Lock()
		entry.running = false
		c.inventoryMu.Unlock()
		_, err := c.ListCommands(ListCommandsRequest{Limit: 1})
		require.ErrorContains(t, err, "invariant")
	})
	t.Run("terminal finish tree missing", func(t *testing.T) {
		c := NewController("", "")
		startedAt := time.Now().UTC().Add(-time.Minute)
		finishedAt := startedAt.Add(time.Second)
		registerTerminalInventory(t, c, "terminal", startedAt, finishedAt)
		c.inventoryMu.Lock()
		entry := c.bySession["terminal"]
		_, _ = c.terminalByFinishedAt.Delete(entry)
		c.inventoryMu.Unlock()
		_, err := c.ListCommands(ListCommandsRequest{Limit: 1})
		require.ErrorContains(t, err, "invariant")
	})
	t.Run("no progress", func(t *testing.T) {
		c, _ := newRunning(t)
		c.inventoryMu.Lock()
		_, err := c.listCommandsLocked(commandCursorFilterAll, 0, nil, time.Now())
		c.inventoryMu.Unlock()
		require.ErrorContains(t, err, "no progress")
	})
}

func testCommandInventoryListPhysicalBudget(t *testing.T) {
	c := NewController("", "", WithCommandInventory(CommandInventoryConfig{RecoveryTTL: time.Hour, MaxTerminal: 400}))
	base := time.Now().UTC().Add(-2 * time.Hour)
	for i := range 260 {
		registerTerminalInventory(t, c, fmt.Sprintf("expired-%03d", i), base.Add(time.Duration(i)*time.Nanosecond), base)
	}
	mustRegisterRunning(t, c, inventoryEntry{session: "running", running: true, startedAt: base.Add(-time.Hour)})
	response, err := c.ListCommands(ListCommandsRequest{Limit: 1})
	require.NoError(t, err)
	require.Empty(t, response.Commands, "expired terminal winners consume the physical budget before older running data")
	require.NotNil(t, response.Pagination.NextCursor, "the short page must strictly continue past discarded winners")
	following, err := c.ListCommands(ListCommandsRequest{Limit: 1, Cursor: *response.Pagination.NextCursor})
	require.NoError(t, err)
	require.Equal(t, []string{"running"}, commandSummarySessions(following.Commands))
	require.Nil(t, following.Pagination.NextCursor, "cleanup proved that no further entries remain")
}

func TestCommandInventoryCursor(t *testing.T) {
	c := NewController("", "")
	valid := func(payload string) string {
		return base64.RawURLEncoding.EncodeToString([]byte(payload))
	}
	for _, cursor := range []string{
		"%", "=", valid(`[]`), valid(`{"version":1,"process":"` + c.inventoryProcessID + `","filter":"all","sort":"started_at_desc_session_desc","extra":true}`),
		valid(`{"version":1,"process":"` + c.inventoryProcessID + `","filter":"all","sort":"started_at_desc_session_desc"} trailing`),
		valid(`{"version":2,"process":"` + c.inventoryProcessID + `","filter":"all","sort":"started_at_desc_session_desc"}`),
		valid(`{"version":1,"process":"other","filter":"all","sort":"started_at_desc_session_desc"}`),
		valid(`{"version":1,"process":"` + c.inventoryProcessID + `","filter":"other","sort":"started_at_desc_session_desc"}`),
		valid(`{"version":1,"process":"` + c.inventoryProcessID + `","filter":"all","sort":"other"}`),
		valid(`{"version":1,"process":"` + c.inventoryProcessID + `","filter":"all","sort":"started_at_desc_session_desc","frontier":{"started_at_unix_nano":1}}`),
		valid(`{"version":1,"process":"` + c.inventoryProcessID + `","filter":"all","sort":"started_at_desc_session_desc","frontier":{"started_at_unix_nano":1,"session":""}}`),
		valid(`{"version":1,"process":"` + c.inventoryProcessID + `","filter":"all","sort":"started_at_desc_session_desc"}`),
		valid(`{"version":1,"process":"` + c.inventoryProcessID + `","filter":"all","sort":"started_at_desc_session_desc","frontier":null}`),
		valid(`{"version":1,"process":"`+c.inventoryProcessID+`","filter":"all","sort":"started_at_desc_session_desc","frontier":{"started_at_unix_nano":1,"session":"a"}}`) + "\n",
		string(make([]byte, 1367)),
	} {
		_, err := c.ListCommands(ListCommandsRequest{Cursor: cursor})
		if !errors.Is(err, ErrInvalidCommandCursor) {
			t.Fatalf("cursor %q error = %v, want invalid cursor", cursor, err)
		}
	}
}

func TestCommandInventoryRetention(t *testing.T) {
	t.Run("cap eviction and cleanup", testCommandInventoryRetentionCapEvictionAndCleanup)
	t.Run("exact TTL boundary", testCommandInventoryRetentionTTLBoundary)
	t.Run("cleanup batch removes map and index identities", testCommandInventoryCleanupExpiredRemovesMatchingIdentities)
	t.Run("concurrent nonowner skips active cleanup", testCommandInventoryRetentionConcurrentNonownerSkipsCleanup)
	t.Run("later request gets a fresh cleanup pass", testCommandInventoryRetentionLaterRequestGetsFreshCleanupPass)
	t.Run("later list cleans newly expired entry", testCommandInventoryRetentionLaterListCleansNewlyExpiredEntry)
}

func testCommandInventoryRetentionCapEvictionAndCleanup(t *testing.T) {
	c := NewController("", "", WithCommandInventory(CommandInventoryConfig{
		RecoveryTTL: time.Hour,
		MaxTerminal: 1,
	}))
	base := time.Now().UTC().Add(-2 * time.Hour)
	for i := range 2 {
		session := fmt.Sprintf("terminal-%d", i)
		startedAt := base.Add(time.Duration(i) * time.Minute)
		finishedAt := startedAt.Add(time.Second)
		mustRegisterRunning(t, c, inventoryEntry{session: session, running: true, startedAt: startedAt})
		mustTransitionTerminal(t, c, inventoryEntry{session: session, startedAt: startedAt, finishedAt: &finishedAt})
	}
	c.inventoryMu.Lock()
	require.Equal(t, 1, c.terminalByFinishedAt.Len())
	c.commandInventoryConfig.MaxTerminal = 10
	c.inventoryMu.Unlock()

	response, err := c.ListCommands(ListCommandsRequest{Limit: 1})
	require.NoError(t, err)
	require.Empty(t, response.Commands)
	c.inventoryMu.Lock()
	require.Zero(t, c.terminalByFinishedAt.Len())
	c.inventoryMu.Unlock()
}

func testCommandInventoryRetentionTTLBoundary(t *testing.T) {
	c := NewController("", "", WithCommandInventory(CommandInventoryConfig{RecoveryTTL: time.Hour, MaxTerminal: 10}))
	finishedAt := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	registerTerminalInventory(t, c, "terminal", finishedAt.Add(-time.Second), finishedAt)
	c.inventoryMu.Lock()
	deleted, err := c.cleanupExpiredLocked(finishedAt.Add(time.Hour).Add(-time.Nanosecond))
	require.NoError(t, err)
	require.Zero(t, deleted)
	deleted, err = c.cleanupExpiredLocked(finishedAt.Add(time.Hour))
	c.inventoryMu.Unlock()
	require.NoError(t, err)
	require.Equal(t, 1, deleted)
}

func testCommandInventoryCleanupExpiredRemovesMatchingIdentities(t *testing.T) {
	c := NewController("", "", WithCommandInventory(CommandInventoryConfig{RecoveryTTL: time.Hour, MaxTerminal: 10}))
	base := time.Now().UTC().Add(-2 * time.Hour)
	registerTerminalInventory(t, c, "expired-a", base, base)
	registerTerminalInventory(t, c, "expired-b", base.Add(time.Nanosecond), base.Add(time.Nanosecond))

	c.inventoryMu.Lock()
	deleted, err := c.cleanupExpiredLocked(time.Now().UTC())
	assertCommandInventoryInvariantLocked(t, c)
	terminalStarted := c.terminalByStartedAt.Len()
	terminalFinished := c.terminalByFinishedAt.Len()
	bySession := len(c.bySession)
	c.inventoryMu.Unlock()
	require.NoError(t, err)
	require.Equal(t, 2, deleted)
	require.Zero(t, terminalStarted)
	require.Zero(t, terminalFinished)
	require.Zero(t, bySession)
}

func testCommandInventoryRetentionConcurrentNonownerSkipsCleanup(t *testing.T) {
	c := NewController("", "", WithCommandInventory(CommandInventoryConfig{RecoveryTTL: time.Hour, MaxTerminal: 300}))
	base := time.Now().UTC().Add(-2 * time.Hour)
	for i := range 200 {
		registerTerminalInventory(t, c, fmt.Sprintf("expired-%03d", i), base.Add(time.Duration(i)*time.Nanosecond), base)
	}

	running := true
	c.cleanupOwner.Store(true)
	c.inventoryMu.Lock()
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		_, err := c.ListCommands(ListCommandsRequest{Running: &running, Limit: 1})
		done <- err
	}()
	<-started
	c.inventoryMu.Unlock()
	require.NoError(t, <-done)

	c.inventoryMu.Lock()
	require.Equal(t, 200, c.terminalByFinishedAt.Len())
	assertCommandInventoryInvariantLocked(t, c)
	c.inventoryMu.Unlock()
	c.cleanupOwner.Store(false)
}

func testCommandInventoryRetentionLaterRequestGetsFreshCleanupPass(t *testing.T) {
	c := NewController("", "", WithCommandInventory(CommandInventoryConfig{RecoveryTTL: time.Hour, MaxTerminal: 300}))
	base := time.Now().UTC().Add(-2 * time.Hour)
	for i := range 200 {
		registerTerminalInventory(t, c, fmt.Sprintf("expired-%03d", i), base.Add(time.Duration(i)*time.Nanosecond), base)
	}

	running := true
	_, err := c.ListCommands(ListCommandsRequest{Running: &running, Limit: 1})
	require.NoError(t, err)
	c.inventoryMu.Lock()
	require.Equal(t, 72, c.terminalByFinishedAt.Len())
	assertCommandInventoryInvariantLocked(t, c)
	c.inventoryMu.Unlock()

	_, err = c.ListCommands(ListCommandsRequest{Running: &running, Limit: 1})
	require.NoError(t, err)
	c.inventoryMu.Lock()
	defer c.inventoryMu.Unlock()
	require.Zero(t, c.terminalByFinishedAt.Len())
	assertCommandInventoryInvariantLocked(t, c)
}

func testCommandInventoryRetentionLaterListCleansNewlyExpiredEntry(t *testing.T) {
	c := NewController("", "", WithCommandInventory(CommandInventoryConfig{RecoveryTTL: time.Hour, MaxTerminal: 10}))
	startedAt := time.Now().UTC().Add(-time.Minute)
	finishedAt := time.Now().UTC()
	registerTerminalInventory(t, c, "terminal", startedAt, finishedAt)

	running := true
	_, err := c.ListCommands(ListCommandsRequest{Running: &running, Limit: 1})
	require.NoError(t, err)
	c.inventoryMu.Lock()
	require.Equal(t, 1, c.terminalByFinishedAt.Len())
	assertCommandInventoryInvariantLocked(t, c)
	entry := c.bySession["terminal"]
	expiredAt := time.Now().UTC().Add(-2 * time.Hour)
	entry.finishedAt = &expiredAt
	c.inventoryMu.Unlock()

	_, err = c.ListCommands(ListCommandsRequest{Running: &running, Limit: 1})
	require.NoError(t, err)
	c.inventoryMu.Lock()
	defer c.inventoryMu.Unlock()
	require.Zero(t, c.terminalByFinishedAt.Len())
	assertCommandInventoryInvariantLocked(t, c)
}

func TestCommandInventoryConcurrentLifecycleAndList(t *testing.T) {
	const (
		writers           = 2
		commandsPerWriter = 100
		readers           = 2
	)
	c := NewController("", "", WithCommandInventory(CommandInventoryConfig{
		RecoveryTTL: time.Hour,
		MaxTerminal: writers * commandsPerWriter,
	}))
	base := time.Now().UTC().Add(-time.Minute)
	sessions := make([]string, 0, writers*commandsPerWriter)
	for writer := range writers {
		for command := range commandsPerWriter {
			session := fmt.Sprintf("writer-%d-command-%03d", writer, command)
			sessions = append(sessions, session)
			c.storeCommandKernel(session, &commandKernel{running: true, startedAt: base})
		}
	}

	start := make(chan struct{})
	stop := make(chan struct{})
	errs := make(chan error, writers+readers+1)
	var writersDone sync.WaitGroup
	var workers sync.WaitGroup
	report := func(err error) {
		select {
		case errs <- err:
		default:
		}
	}

	for writer := range writers {
		workers.Add(1)
		writersDone.Add(1)
		go func(writer int) {
			defer workers.Done()
			defer writersDone.Done()
			<-start
			for command := range commandsPerWriter {
				session := fmt.Sprintf("writer-%d-command-%03d", writer, command)
				startedAt := base.Add(time.Duration(writer*commandsPerWriter+command) * time.Nanosecond)
				if err := c.registerRunningCommand(inventoryEntry{session: session, running: true, startedAt: startedAt}); err != nil {
					report(fmt.Errorf("register %s: %w", session, err))
					return
				}
				finishedAt := startedAt.Add(time.Nanosecond)
				if err := c.transitionCommandTerminal(inventoryEntry{session: session, startedAt: startedAt, finishedAt: &finishedAt}); err != nil {
					report(fmt.Errorf("transition %s: %w", session, err))
					return
				}
			}
		}(writer)
	}

	for range readers {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := c.ListCommands(ListCommandsRequest{Limit: 25}); err != nil {
					report(fmt.Errorf("list commands: %w", err))
					return
				}
			}
		}()
	}

	workers.Add(1)
	go func() {
		defer workers.Done()
		<-start
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := c.GetCommandStatus(sessions[0]); err != nil {
				report(fmt.Errorf("legacy command status: %w", err))
				return
			}
		}
	}()

	close(start)
	writersDone.Wait()
	close(stop)
	workers.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	c.inventoryMu.Lock()
	defer c.inventoryMu.Unlock()
	require.Zero(t, c.runningByStartedAt.Len())
	require.Equal(t, writers*commandsPerWriter, c.terminalByStartedAt.Len())
	require.Equal(t, writers*commandsPerWriter, c.terminalByFinishedAt.Len())
	require.Len(t, c.bySession, writers*commandsPerWriter)
	assertCommandInventoryInvariantLocked(t, c)
}

func registerTerminalInventory(t *testing.T, c *Controller, session string, startedAt, finishedAt time.Time) {
	t.Helper()
	mustRegisterRunning(t, c, inventoryEntry{session: session, running: true, startedAt: startedAt})
	mustTransitionTerminal(t, c, inventoryEntry{session: session, startedAt: startedAt, finishedAt: &finishedAt})
}

func commandSummarySessions(commands []CommandSummary) []string {
	sessions := make([]string, 0, len(commands))
	for _, command := range commands {
		sessions = append(sessions, command.Session)
	}
	return sessions
}

func TestCommandInventoryStateMachine(t *testing.T) {
	startedAt := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	finishedAt := startedAt.Add(time.Minute)
	exitCode := 17
	running := inventoryEntry{
		session:    "session-1",
		running:    true,
		background: true,
		startedAt:  startedAt,
	}
	terminal := inventoryEntry{
		session:    running.session,
		background: running.background,
		startedAt:  startedAt,
		finishedAt: &finishedAt,
		exitCode:   &exitCode,
		errMsg:     "command failed",
	}

	newController := func(config CommandInventoryConfig) *Controller {
		return NewController("", "", WithCommandInventory(config))
	}
	retainedConfig := CommandInventoryConfig{RecoveryTTL: time.Hour, MaxTerminal: 1000}

	t.Run("absent to running", func(t *testing.T) {
		c := newController(retainedConfig)
		if err := c.registerRunningCommand(running); err != nil {
			t.Fatalf("register running command: %v", err)
		}
		entry := c.bySession[running.session]
		if entry == nil || entry == &running || !entry.running {
			t.Fatal("running command was not stored as a running snapshot")
		}
		assertInventoryIndexes(t, c, entry, 1, 0, 0)
		assertIndexIdentity(t, c.runningByStartedAt, entry, "running")
	})

	t.Run("running to terminal", func(t *testing.T) {
		c := newController(retainedConfig)
		mustRegisterRunning(t, c, running)
		runningEntry := c.bySession[running.session]
		if err := c.transitionCommandTerminal(terminal); err != nil {
			t.Fatalf("transition command terminal: %v", err)
		}
		entry := c.bySession[running.session]
		if entry == nil || entry == runningEntry || entry.running {
			t.Fatal("terminal command did not replace the running map identity")
		}
		assertInventoryIndexes(t, c, entry, 0, 1, 1)
		if _, ok := c.runningByStartedAt.Get(runningEntry); ok {
			t.Fatal("running index retained the replaced running entry")
		}
		assertIndexIdentity(t, c.terminalByStartedAt, entry, "terminal start")
		assertIndexIdentity(t, c.terminalByFinishedAt, entry, "terminal finish")
	})

	t.Run("duplicate terminal is a no-op", func(t *testing.T) {
		c := newController(retainedConfig)
		mustRegisterRunning(t, c, running)
		mustTransitionTerminal(t, c, terminal)
		entry := c.bySession[running.session]
		sequence := c.terminalSequence
		if err := c.transitionCommandTerminal(terminal); err != nil {
			t.Fatalf("duplicate terminal transition: %v", err)
		}
		if c.bySession[running.session] != entry || c.terminalSequence != sequence {
			t.Fatal("duplicate terminal transition changed map identity or sequence")
		}
		assertInventoryIndexes(t, c, entry, 0, 1, 1)
		assertIndexIdentity(t, c.terminalByStartedAt, entry, "terminal start")
		assertIndexIdentity(t, c.terminalByFinishedAt, entry, "terminal finish")
	})

	t.Run("absent to terminal is rejected without index writes", func(t *testing.T) {
		c := newController(retainedConfig)
		if err := c.transitionCommandTerminal(terminal); err == nil {
			t.Fatal("transitioning an absent command to terminal succeeded")
		}
		if _, ok := c.bySession[running.session]; ok {
			t.Fatal("absent terminal transition inserted a map entry")
		}
		assertInventoryIndexes(t, c, nil, 0, 0, 0)
	})

	t.Run("terminal to running is rejected without index writes", func(t *testing.T) {
		c := newController(retainedConfig)
		mustRegisterRunning(t, c, running)
		mustTransitionTerminal(t, c, terminal)
		entry := c.bySession[running.session]
		sequence := c.terminalSequence
		if err := c.registerRunningCommand(running); err == nil {
			t.Fatal("transitioning a terminal command to running succeeded")
		}
		if c.bySession[running.session] != entry || c.terminalSequence != sequence {
			t.Fatal("terminal to running changed map identity or sequence")
		}
		assertInventoryIndexes(t, c, entry, 0, 1, 1)
	})

	t.Run("mismatched terminal summary is rejected without mutation", func(t *testing.T) {
		c := newController(retainedConfig)
		mustRegisterRunning(t, c, running)
		entry := c.bySession[running.session]
		mismatched := terminal
		mismatched.background = !running.background
		if err := c.transitionCommandTerminal(mismatched); err == nil {
			t.Fatal("mismatched terminal summary succeeded")
		}
		if c.bySession[running.session] != entry || c.terminalSequence != 0 {
			t.Fatal("mismatched terminal summary changed the running state")
		}
		assertInventoryIndexes(t, c, entry, 1, 0, 0)
	})

	t.Run("terminal without finished time is rejected without mutation", func(t *testing.T) {
		c := newController(retainedConfig)
		mustRegisterRunning(t, c, running)
		entry := c.bySession[running.session]
		invalid := terminal
		invalid.finishedAt = nil
		if err := c.transitionCommandTerminal(invalid); err == nil {
			t.Fatal("terminal summary without finished time succeeded")
		}
		if c.bySession[running.session] != entry || c.terminalSequence != 0 {
			t.Fatal("invalid terminal summary changed the running state")
		}
		assertInventoryIndexes(t, c, entry, 1, 0, 0)
	})

	for _, test := range []struct {
		name   string
		config CommandInventoryConfig
	}{
		{name: "zero TTL", config: CommandInventoryConfig{RecoveryTTL: 0, MaxTerminal: 1000}},
		{name: "zero cap", config: CommandInventoryConfig{RecoveryTTL: time.Hour, MaxTerminal: 0}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := newController(test.config)
			mustRegisterRunning(t, c, running)
			mustTransitionTerminal(t, c, terminal)
			if _, ok := c.bySession[running.session]; ok {
				t.Fatal("zero retention terminal transition did not delete the map entry")
			}
			assertInventoryIndexes(t, c, nil, 0, 0, 0)
			if err := c.transitionCommandTerminal(terminal); err == nil {
				t.Fatal("duplicate zero retention terminal transition succeeded")
			}
			if _, ok := c.bySession[running.session]; ok {
				t.Fatal("duplicate terminal transition resurrected the map entry")
			}
			assertInventoryIndexes(t, c, nil, 0, 0, 0)
		})
	}

	t.Run("source index inconsistency does not mutate further", func(t *testing.T) {
		c := newController(retainedConfig)
		mustRegisterRunning(t, c, running)
		entry := c.bySession[running.session]
		if _, ok := c.runningByStartedAt.Delete(entry); !ok {
			t.Fatal("delete running index entry")
		}
		if err := c.transitionCommandTerminal(terminal); err == nil {
			t.Fatal("transition with missing running index succeeded")
		}
		if c.bySession[running.session] != entry || c.terminalSequence != 0 {
			t.Fatal("transition with missing running index mutated map or sequence")
		}
		assertInventoryIndexes(t, c, nil, 0, 0, 0)
	})

	t.Run("target index inconsistency does not mutate running state", func(t *testing.T) {
		c := newController(retainedConfig)
		mustRegisterRunning(t, c, running)
		entry := c.bySession[running.session]
		blocker := &inventoryEntry{
			session:          running.session,
			startedAt:        running.startedAt,
			finishedAt:       &finishedAt,
			terminalSequence: 1,
		}
		if _, replaced := c.terminalByStartedAt.Set(blocker); replaced {
			t.Fatal("set target index blocker replaced an entry")
		}
		if err := c.transitionCommandTerminal(terminal); err == nil {
			t.Fatal("transition with occupied target index succeeded")
		}
		if c.bySession[running.session] != entry || c.terminalSequence != 0 {
			t.Fatal("transition with occupied target index mutated map or sequence")
		}
		assertIndexIdentity(t, c.runningByStartedAt, entry, "running")
		assertIndexIdentity(t, c.terminalByStartedAt, blocker, "target blocker")
		if c.runningByStartedAt.Len() != 1 || c.terminalByStartedAt.Len() != 1 || c.terminalByFinishedAt.Len() != 0 {
			t.Fatal("transition with occupied target index changed tree lengths")
		}
	})

	t.Run("terminal delete preflight prevents partial deletion", func(t *testing.T) {
		c := newController(retainedConfig)
		mustRegisterRunning(t, c, running)
		mustTransitionTerminal(t, c, terminal)
		entry := c.bySession[running.session]
		if _, ok := c.terminalByFinishedAt.Delete(entry); !ok {
			t.Fatal("delete terminal finish index entry")
		}
		if err := c.deleteTerminalLocked(entry); err == nil {
			t.Fatal("terminal delete with missing finish index succeeded")
		}
		if c.bySession[running.session] != entry {
			t.Fatal("terminal delete with missing finish index changed map identity")
		}
		assertIndexIdentity(t, c.terminalByStartedAt, entry, "terminal start")
		if c.runningByStartedAt.Len() != 0 || c.terminalByStartedAt.Len() != 1 || c.terminalByFinishedAt.Len() != 0 {
			t.Fatal("terminal delete preflight changed tree lengths")
		}
	})

	t.Run("index key contracts", testCommandInventoryIndexKeys)
}

func testCommandInventoryIndexKeys(t *testing.T) {
	c := NewController("", "")
	startedAt := time.Unix(0, 42).UTC()
	finishedAt := time.Unix(0, 84).UTC()

	runningB := &inventoryEntry{session: "b", startedAt: startedAt}
	runningA := &inventoryEntry{session: "a", startedAt: startedAt}
	if _, replaced := c.runningByStartedAt.Set(runningB); replaced {
		t.Fatal("running B unexpectedly replaced an entry")
	}
	if _, replaced := c.runningByStartedAt.Set(runningA); replaced {
		t.Fatal("running A unexpectedly replaced an entry")
	}
	assertIndexSessions(t, c.runningByStartedAt, []string{"a", "b"})
	if previous, replaced := c.runningByStartedAt.Set(&inventoryEntry{session: "a", startedAt: startedAt}); !replaced || previous != runningA {
		t.Fatal("running index did not use exactly startedAt/session as its unique key")
	}

	terminalStartB := &inventoryEntry{session: "b", startedAt: startedAt, finishedAt: &finishedAt, terminalSequence: 2}
	terminalStartA := &inventoryEntry{session: "a", startedAt: startedAt, finishedAt: &finishedAt, terminalSequence: 3}
	if _, replaced := c.terminalByStartedAt.Set(terminalStartB); replaced {
		t.Fatal("terminal start B unexpectedly replaced an entry")
	}
	if _, replaced := c.terminalByStartedAt.Set(terminalStartA); replaced {
		t.Fatal("terminal start A unexpectedly replaced an entry")
	}
	assertIndexSessions(t, c.terminalByStartedAt, []string{"a", "b"})
	if previous, replaced := c.terminalByStartedAt.Set(&inventoryEntry{session: "a", startedAt: startedAt, finishedAt: &finishedAt, terminalSequence: 99}); !replaced || previous != terminalStartA {
		t.Fatal("terminal start index did not use exactly startedAt/session as its unique key")
	}

	terminalFinishSequenceTwo := &inventoryEntry{session: "a", startedAt: startedAt, finishedAt: &finishedAt, terminalSequence: 2}
	terminalFinishSequenceOne := &inventoryEntry{session: "z", startedAt: startedAt, finishedAt: &finishedAt, terminalSequence: 1}
	if _, replaced := c.terminalByFinishedAt.Set(terminalFinishSequenceTwo); replaced {
		t.Fatal("terminal finish sequence two unexpectedly replaced an entry")
	}
	if _, replaced := c.terminalByFinishedAt.Set(terminalFinishSequenceOne); replaced {
		t.Fatal("terminal finish sequence one unexpectedly replaced an entry")
	}
	assertIndexSessions(t, c.terminalByFinishedAt, []string{"z", "a"})
	if previous, replaced := c.terminalByFinishedAt.Set(&inventoryEntry{session: "other", startedAt: startedAt, finishedAt: &finishedAt, terminalSequence: 1}); !replaced || previous != terminalFinishSequenceOne {
		t.Fatal("terminal finish index did not use exactly finishedAt/terminalSequence as its unique key")
	}
}

func TestCommandInventoryProcessIdentity(t *testing.T) {
	c := NewController("", "")
	originalID := c.inventoryProcessID
	if originalID == "" {
		t.Fatal("controller process identity is empty")
	}
	startedAt := time.Now().UTC()
	finishedAt := startedAt.Add(time.Second)
	running := inventoryEntry{session: "session-1", running: true, startedAt: startedAt}
	terminal := inventoryEntry{session: running.session, startedAt: startedAt, finishedAt: &finishedAt}
	mustRegisterRunning(t, c, running)
	mustTransitionTerminal(t, c, terminal)
	if c.inventoryProcessID != originalID {
		t.Fatal("controller process identity changed during a command transition")
	}

	other := NewController("", "")
	if other.inventoryProcessID == "" {
		t.Fatal("second controller process identity is empty")
	}
	if c.inventoryProcessID == other.inventoryProcessID {
		t.Fatal("controllers share a process-local inventory identity")
	}
}

func mustRegisterRunning(t *testing.T, c *Controller, summary inventoryEntry) {
	t.Helper()
	if err := c.registerRunningCommand(summary); err != nil {
		t.Fatalf("register running command: %v", err)
	}
}

func mustTransitionTerminal(t *testing.T, c *Controller, summary inventoryEntry) {
	t.Helper()
	if err := c.transitionCommandTerminal(summary); err != nil {
		t.Fatalf("transition terminal command: %v", err)
	}
}

func assertInventoryIndexes(t *testing.T, c *Controller, entry *inventoryEntry, running, terminalStarted, terminalFinished int) {
	t.Helper()
	if c.runningByStartedAt.Len() != running || c.terminalByStartedAt.Len() != terminalStarted || c.terminalByFinishedAt.Len() != terminalFinished {
		t.Fatalf("unexpected index lengths: running=%d terminal-start=%d terminal-finish=%d", c.runningByStartedAt.Len(), c.terminalByStartedAt.Len(), c.terminalByFinishedAt.Len())
	}
	if entry == nil {
		return
	}
	if entry.running {
		assertIndexIdentity(t, c.runningByStartedAt, entry, "running")
		return
	}
	assertIndexIdentity(t, c.terminalByStartedAt, entry, "terminal start")
	assertIndexIdentity(t, c.terminalByFinishedAt, entry, "terminal finish")
}

func assertIndexIdentity(t *testing.T, index interface {
	Get(*inventoryEntry) (*inventoryEntry, bool)
}, entry *inventoryEntry, name string) {
	t.Helper()
	stored, ok := index.Get(entry)
	if !ok || stored != entry {
		t.Fatalf("%s index does not contain the expected identity", name)
	}
}

func assertIndexSessions(t *testing.T, index interface {
	Scan(func(*inventoryEntry) bool)
}, expected []string) {
	t.Helper()
	var actual []string
	index.Scan(func(entry *inventoryEntry) bool {
		actual = append(actual, entry.session)
		return true
	})
	if len(actual) != len(expected) {
		t.Fatalf("unexpected index scan length: got %v, want %v", actual, expected)
	}
	for i := range expected {
		if actual[i] != expected[i] {
			t.Fatalf("unexpected index scan: got %v, want %v", actual, expected)
		}
	}
}

// assertCommandInventoryInvariantLocked verifies every map entry has exactly
// one state-appropriate index identity. Callers must hold inventoryMu.
func assertCommandInventoryInvariantLocked(t *testing.T, c *Controller) {
	t.Helper()
	running := make(map[string]*inventoryEntry, c.runningByStartedAt.Len())
	c.runningByStartedAt.Scan(func(entry *inventoryEntry) bool {
		if entry == nil {
			t.Fatal("nil running index entry")
		}
		if !entry.running || running[entry.session] != nil {
			t.Fatalf("invalid or duplicate running index entry for %q", entry.session)
		}
		running[entry.session] = entry
		return true
	})
	terminalStarted := make(map[string]*inventoryEntry, c.terminalByStartedAt.Len())
	c.terminalByStartedAt.Scan(func(entry *inventoryEntry) bool {
		if entry == nil {
			t.Fatal("nil terminal-start index entry")
		}
		if entry.running || entry.finishedAt == nil || terminalStarted[entry.session] != nil {
			t.Fatalf("invalid or duplicate terminal-start index entry for %q", entry.session)
		}
		terminalStarted[entry.session] = entry
		return true
	})
	terminalFinished := make(map[string]*inventoryEntry, c.terminalByFinishedAt.Len())
	c.terminalByFinishedAt.Scan(func(entry *inventoryEntry) bool {
		if entry == nil {
			t.Fatal("nil terminal-finish index entry")
		}
		if entry.running || entry.finishedAt == nil || terminalFinished[entry.session] != nil {
			t.Fatalf("invalid or duplicate terminal-finish index entry for %q", entry.session)
		}
		terminalFinished[entry.session] = entry
		return true
	})
	if len(c.bySession) != len(running)+len(terminalStarted) || len(terminalStarted) != len(terminalFinished) {
		t.Fatalf("inventory cardinality mismatch: map=%d running=%d terminal-start=%d terminal-finish=%d", len(c.bySession), len(running), len(terminalStarted), len(terminalFinished))
	}
	for session, entry := range c.bySession {
		if entry == nil || entry.session != session || entry.indexSession != session || entry.indexStartedAt != entry.startedAt.UnixNano() {
			t.Fatalf("invalid map entry for %q", session)
		}
		if entry.running {
			if running[session] != entry || terminalStarted[session] != nil || terminalFinished[session] != nil {
				t.Fatalf("running map/index identity mismatch for %q", session)
			}
			continue
		}
		if terminalStarted[session] != entry || terminalFinished[session] != entry || running[session] != nil {
			t.Fatalf("terminal map/index identity mismatch for %q", session)
		}
	}
}

func TestCommandInventoryTerminalSnapshotReleasesControllerLock(t *testing.T) {
	c := NewController("", "")
	startedAt := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	c.storeCommandKernel("session", &commandKernel{
		startedAt: startedAt, running: true, isBackground: true,
	})

	snapshot := c.markCommandFinished("session", 17, "command failed")
	require.Equal(t, "session", snapshot.session)
	require.Equal(t, startedAt, snapshot.startedAt)
	require.Equal(t, 17, snapshot.exitCode)
	require.Equal(t, "command failed", snapshot.errMsg)
	require.True(t, snapshot.background)
	require.False(t, snapshot.finishedAt.IsZero())

	locked := make(chan struct{})
	go func() {
		c.mu.Lock()
		close(locked)
		c.mu.Unlock()
	}()
	select {
	case <-locked:
	case <-time.After(time.Second):
		t.Fatal("markCommandFinished returned with c.mu held")
	}

	c.mu.Lock()
	kernel := c.getCommandKernel("session")
	kernel.startedAt = time.Time{}
	kernel.errMsg = "mutated"
	c.mu.Unlock()
	require.Equal(t, startedAt, snapshot.startedAt)
	require.Equal(t, "command failed", snapshot.errMsg)
}
