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

// Package resolvrewrite rewrites a sandbox resolv.conf so its DNS goes
// through the egress proxy: every nameserver line is replaced with the
// subject gateway; search/domain/options directives are preserved.
//
// Fail-closed direction: if the rewrite cannot be applied (mount not ready,
// unreadable file), the caller must not create the sandbox — a resolv.conf
// pointing at the old nameservers would bypass DNS policy.
package resolvrewrite

import (
	"fmt"
	"net/netip"
	"os"
	"strings"
)

// Rewrite replaces all nameserver lines in content with the gateway and
// returns the rewritten file content.
func Rewrite(content string, gateway netip.Addr) string {
	var out []string
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "nameserver") {
			continue
		}
		out = append(out, line)
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("nameserver %s\n", gateway))
	if len(out) > 0 {
		b.WriteString(strings.Join(out, "\n"))
	}
	return b.String()
}

// RewriteFile rewrites the resolv.conf at path in place. The file must exist
// (fastlet bind-mounts it before the sandbox starts).
func RewriteFile(path string, gateway netip.Addr) error {
	content, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read resolv.conf %s: %w", path, err)
	}
	rewritten := Rewrite(string(content), gateway)
	if err := os.WriteFile(path, []byte(rewritten), 0o644); err != nil {
		return fmt.Errorf("write resolv.conf %s: %w", path, err)
	}
	return nil
}
