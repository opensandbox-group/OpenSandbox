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

package policy

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoadLogSkipFile_ParsesDomainsAndComments(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log_skip.always")
	body := []byte("# comment line\n" +
		"metadata.internal\n" +
		"\n" +
		"*.cluster.local\n" +
		"   # indented comment is also ignored\n" +
		"  Foo.Example.Com  \n")
	require.NoError(t, os.WriteFile(path, body, 0o644))

	patterns, err := loadLogSkipFile(path)
	require.NoError(t, err)
	// Raw user case is preserved here; DomainSet lowercases at compile/match time
	// so downstream matching is still case-insensitive.
	require.Equal(t, []string{"metadata.internal", "*.cluster.local", "Foo.Example.Com"}, patterns,
		"comments and blank lines are dropped; surrounding whitespace stripped; order preserved")
}

func TestLoadLogSkipFile_BlankAndCommentsOnlyReturnsNil(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log_skip.always")
	require.NoError(t, os.WriteFile(path, []byte("# just a comment\n\n   \n# another\n"), 0o644))

	patterns, err := loadLogSkipFile(path)
	require.NoError(t, err)
	require.Nil(t, patterns, "file with no effective entries must produce nil patterns")
}

func TestLoadLogSkipFile_MissingFileIsNotError(t *testing.T) {
	patterns, err := loadLogSkipFile(filepath.Join(t.TempDir(), "does-not-exist"))
	require.NoError(t, err, "missing file is a soft no-op")
	require.Nil(t, patterns)
}

func TestLoadLogSkipFile_RejectsIPEntry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log_skip.always")
	require.NoError(t, os.WriteFile(path, []byte("1.2.3.4\n"), 0o644))

	_, err := loadLogSkipFile(path)
	require.Error(t, err, "IP entries must be rejected — log skip matches DNS query names")
	require.Contains(t, err.Error(), "not a domain pattern")
}

func TestLoadLogSkipFile_RejectsCIDREntry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log_skip.always")
	require.NoError(t, os.WriteFile(path, []byte("10.0.0.0/8\n"), 0o644))

	_, err := loadLogSkipFile(path)
	require.Error(t, err, "CIDR entries must be rejected")
	require.Contains(t, err.Error(), "not a domain pattern")
}

func TestLoadLogSkipFile_ReportsLineNumberOnRejectedEntry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log_skip.always")
	require.NoError(t, os.WriteFile(path, []byte("ok.example.com\n10.0.0.0/8\n"), 0o644))

	_, err := loadLogSkipFile(path)
	require.Error(t, err)
	require.Contains(t, err.Error(), "line 2", "error must point at the offending line")
	require.Contains(t, err.Error(), "not a domain pattern")
}
