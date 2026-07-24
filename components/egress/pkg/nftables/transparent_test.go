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

package nftables

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSetupTransparent_BuildsNativeNftScript(t *testing.T) {
	var scripts []string
	run := func(_ context.Context, script string) ([]byte, error) {
		scripts = append(scripts, script)
		return nil, nil
	}

	err := setupTransparent(context.Background(), run, 8080, 1000)
	require.NoError(t, err)

	// One cleanup delete, then the add-only apply.
	require.Len(t, scripts, 2)
	require.Equal(t, "delete table ip opensandbox_transparent\n", scripts[0])

	add := scripts[1]
	require.Contains(t, add, "add table ip opensandbox_transparent")
	require.Contains(t, add, "type nat hook output priority -100; policy accept;")
	require.Contains(t, add, "ip daddr 127.0.0.0/8 return")
	require.Contains(t, add, "meta skuid != 1000 tcp dport 80 redirect to :8080")
	require.Contains(t, add, "meta skuid != 1000 tcp dport 443 redirect to :8080")
	require.NotContains(t, add, "hook prerouting")
	require.NotContains(t, add, "iptables")
	require.NotContains(t, add, "--uid-owner")
}

func TestSetupTransparent_ToleratesMissingTable(t *testing.T) {
	run := func(_ context.Context, script string) ([]byte, error) {
		if strings.HasPrefix(script, "delete table ") {
			return []byte("Error: Could not process rule: No such file or directory"), errors.New("exit status 1")
		}
		return nil, nil
	}

	require.NoError(t, setupTransparent(context.Background(), run, 8080, 1000))
}

func TestSetupTransparent_ReturnsApplyError(t *testing.T) {
	run := func(_ context.Context, script string) ([]byte, error) {
		if strings.HasPrefix(script, "add table ") {
			return []byte("Error: Could not process rule: Operation not supported"), errors.New("exit status 1")
		}
		return nil, nil
	}

	err := setupTransparent(context.Background(), run, 8080, 1000)
	require.Error(t, err)
	require.Contains(t, err.Error(), "nft transparent apply failed")
}
