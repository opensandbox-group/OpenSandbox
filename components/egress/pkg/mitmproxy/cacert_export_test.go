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

package mitmproxy

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCandidateCACertPaths(t *testing.T) {
	h := "/var/lib/mitmproxy"
	got := candidateCACertPaths("", h)
	require.Equal(t, []string{filepath.Join(h, ".mitmproxy", mitmCACertName)}, got)

	got = candidateCACertPaths("/custom", h)
	require.Equal(t, []string{
		filepath.Join("/custom", mitmCACertName),
		filepath.Join("/custom", ".mitmproxy", mitmCACertName),
		filepath.Join(h, ".mitmproxy", mitmCACertName),
	}, got)
}
