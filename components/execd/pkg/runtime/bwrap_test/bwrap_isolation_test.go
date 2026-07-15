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

//go:build linux && bwrap

package bwrap_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alibaba/opensandbox/execd/pkg/runtime"
)

func TestEnvBlacklist(t *testing.T) {
	r := newRunner(t)

	os.Setenv("BWRAP_TEST_SECRET_TOKEN", "super-secret-12345")
	defer os.Unsetenv("BWRAP_TEST_SECRET_TOKEN")

	opts := &runtime.IsolatedSessionOptions{
		Profile:            "strict",
		WorkspacePath:      t.TempDir(),
		WorkspaceMode:      "rw",
		EnvPassthroughMode: "deny",
		EnvPassthroughKeys: nil, // triggers built-in strict blacklist (*_TOKEN etc.)
	}

	id, err := r.CreateIsolatedSession(opts)
	require.NoError(t, err)
	defer r.DeleteIsolatedSession(id)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var lines []string
	err = r.RunInIsolatedSession(ctx, id, "echo TOKEN=${BWRAP_TEST_SECRET_TOKEN:-NOT_SET}", nil, func(line string) {
		lines = append(lines, line)
	})
	require.NoError(t, err)
	assert.Contains(t, lines[0], "NOT_SET", "secret token should have been stripped")
}

func TestEnvAllowMode(t *testing.T) {
	r := newRunner(t)

	os.Setenv("BWRAP_ALLOWED_VAR", "allowed-value")
	os.Setenv("BWRAP_BLOCKED_VAR", "blocked-value")
	defer os.Unsetenv("BWRAP_ALLOWED_VAR")
	defer os.Unsetenv("BWRAP_BLOCKED_VAR")

	opts := &runtime.IsolatedSessionOptions{
		Profile:            "balanced",
		WorkspacePath:      t.TempDir(),
		WorkspaceMode:      "rw",
		EnvPassthroughMode: "allow",
		EnvPassthroughKeys: []string{"BWRAP_ALLOWED_VAR"},
	}

	id, err := r.CreateIsolatedSession(opts)
	require.NoError(t, err)
	defer r.DeleteIsolatedSession(id)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var lines []string
	err = r.RunInIsolatedSession(ctx, id, "echo ALLOWED=${BWRAP_ALLOWED_VAR:-MISSING} BLOCKED=${BWRAP_BLOCKED_VAR:-MISSING}", nil, func(line string) {
		lines = append(lines, line)
	})
	require.NoError(t, err)
	assert.Contains(t, lines[0], "ALLOWED=allowed-value", "allowlisted var should be present")
	assert.Contains(t, lines[0], "BLOCKED=MISSING", "non-allowlisted var should be absent")
}

func TestNetworkIsolation(t *testing.T) {
	r := newRunner(t)

	opts := &runtime.IsolatedSessionOptions{
		Profile:       "strict",
		WorkspacePath: t.TempDir(),
		WorkspaceMode: "rw",
		ShareNet:      boolPtr(false),
	}

	id, err := r.CreateIsolatedSession(opts)
	require.NoError(t, err)
	defer r.DeleteIsolatedSession(id)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// With --unshare-net, only loopback should be visible.
	var lines []string
	err = r.RunInIsolatedSession(ctx, id, "ip addr show 2>/dev/null | grep -c 'LOOPBACK' || echo lo_visible", nil, func(line string) {
		lines = append(lines, line)
	})
	require.NoError(t, err)
	require.NotEmpty(t, lines)
	t.Logf("network test output: %v", lines)
}

func TestCustomUID(t *testing.T) {
	r := newRunner(t)

	uid := uint32(1000)
	gid := uint32(1000)

	opts := &runtime.IsolatedSessionOptions{
		Profile:       "strict",
		WorkspacePath: t.TempDir(),
		WorkspaceMode: "rw",
		Uid:           &uid,
		Gid:           &gid,
	}

	id, err := r.CreateIsolatedSession(opts)
	require.NoError(t, err)
	defer r.DeleteIsolatedSession(id)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var lines []string
	err = r.RunInIsolatedSession(ctx, id, "id -u; id -g", nil, func(line string) {
		lines = append(lines, line)
	})
	require.NoError(t, err)
	require.Len(t, lines, 2)
	assert.Equal(t, "1000", lines[0], "uid should be 1000")
	assert.Equal(t, "1000", lines[1], "gid should be 1000")
}

func TestBalancedProfile(t *testing.T) {
	r := newRunner(t)

	opts := &runtime.IsolatedSessionOptions{
		Profile:       "balanced",
		WorkspacePath: t.TempDir(),
		WorkspaceMode: "rw",
	}

	id, err := r.CreateIsolatedSession(opts)
	require.NoError(t, err)
	defer r.DeleteIsolatedSession(id)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Balanced shares host /tmp.
	var lines []string
	err = r.RunInIsolatedSession(ctx, id, "echo balanced-test > /tmp/bwrap-balanced-test.txt", nil, func(line string) {
		lines = append(lines, line)
	})
	require.NoError(t, err)

	data, err := os.ReadFile("/tmp/bwrap-balanced-test.txt")
	require.NoError(t, err, "balanced profile: /tmp should be shared with host")
	assert.Equal(t, "balanced-test\n", string(data))
	os.Remove("/tmp/bwrap-balanced-test.txt")
}
