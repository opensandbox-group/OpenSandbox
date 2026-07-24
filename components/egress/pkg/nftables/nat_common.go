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
	"fmt"
	"strings"
)

// deleteTableIfPresent deletes `<family> <name>` via a standalone nft call,
// treating a missing table as success. Setup paths call this before applying an
// add-only ruleset so re-runs are idempotent without relying on the atomic
// delete-then-add error dance (a delete of a non-existent table aborts the whole
// batch under nft's atomic semantics).
func deleteTableIfPresent(ctx context.Context, run runner, family, name string) error {
	script := fmt.Sprintf("delete table %s %s\n", family, name)
	output, err := run(ctx, script)
	if err == nil || isMissingTableDelete(output, err) {
		return nil
	}
	return fmt.Errorf("nft delete table %s %s failed: %w (output: %s)", family, name, err, strings.TrimSpace(string(output)))
}

// isMissingTableDelete reports whether a `delete table` failure was caused by the
// table not existing (kernel returns ENOENT -> "No such file or directory").
func isMissingTableDelete(output []byte, err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error() + " " + string(output))
	return strings.Contains(msg, "no such file") || strings.Contains(msg, "does not exist")
}
