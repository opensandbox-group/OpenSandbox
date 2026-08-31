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

package resolvrewrite

import (
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRewrite(t *testing.T) {
	gateway := netip.MustParseAddr("10.0.0.1")
	in := "nameserver 10.96.0.10\nnameserver 8.8.8.8\nsearch svc.local\noptions timeout:1 attempts:2\n"
	out := Rewrite(in, gateway)
	require.Equal(t, "nameserver 10.0.0.1\nsearch svc.local\noptions timeout:1 attempts:2\n", out)
}

func TestRewriteIdempotent(t *testing.T) {
	gateway := netip.MustParseAddr("10.0.0.1")
	first := Rewrite("nameserver 10.96.0.10\n", gateway)
	assert.Equal(t, first, Rewrite(first, gateway), "rewriting twice must not duplicate the gateway")
}
