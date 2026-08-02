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
	"strings"
	"testing"
)

func FuzzCommandInventoryCursor(f *testing.F) {
	process := "process"
	valid := func(payload string) string {
		return base64.RawURLEncoding.EncodeToString([]byte(payload))
	}
	malformed := make(map[string]bool)
	addMalformed := func(cursor string) {
		malformed[cursor] = true
		f.Add(cursor)
	}
	f.Add("")
	addMalformed(strings.Repeat("a", 1366))
	addMalformed(strings.Repeat("a", 1367))
	addMalformed(base64.RawURLEncoding.EncodeToString(make([]byte, 1024)))
	addMalformed(base64.RawURLEncoding.EncodeToString(make([]byte, 1025)))
	addMalformed(valid(`{"version":1,"process":"process","filter":"all","sort":"started_at_desc_session_desc"} trailing`))
	addMalformed(valid(`{"version":1,"process":"process","filter":"all","sort":"started_at_desc_session_desc","extra":true}`))
	addMalformed(valid(`{"version":1,"process":"process","filter":"all","sort":"started_at_desc_session_desc"}`))
	addMalformed(valid(`{"version":1,"process":"process","filter":"all","sort":"started_at_desc_session_desc","frontier":null}`))
	addMalformed(valid(`{"version":1,"process":"process","filter":"all","sort":"started_at_desc_session_desc","frontier":{"started_at_unix_nano":1,"session":"a"}}`) + "\n")
	addMalformed(valid(`{"version":1,"process":"process","filter":"all","sort":"started_at_desc_session_desc","frontier":{"started_at_unix_nano":1,"session":"a"}}`) + "\r")

	f.Fuzz(func(t *testing.T, cursor string) {
		c := NewController("", "")
		c.inventoryProcessID = process
		_, err := c.ListCommands(ListCommandsRequest{Cursor: cursor})
		if err != nil && !errors.Is(err, ErrInvalidCommandCursor) {
			t.Fatalf("unexpected cursor error: %v", err)
		}
		if malformed[cursor] && !errors.Is(err, ErrInvalidCommandCursor) {
			t.Fatalf("malformed cursor error = %v, want invalid cursor", err)
		}
	})
}
