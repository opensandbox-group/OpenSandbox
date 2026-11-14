// Copyright 2026 The OpenSandbox Authors
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

import "unicode/utf8"

// maxScanLineBytes bounds the size of one piece of an output line. A longer
// line is delivered in pieces instead of failing the read, so the pipe keeps
// draining and the markers that follow are still seen.
const maxScanLineBytes = 16 * 1024 * 1024

// lastRuneBoundary returns the largest n such that b[:n] does not end inside
// a UTF-8 sequence.
func lastRuneBoundary(b []byte) int {
	for i := len(b) - 1; i >= 0 && i >= len(b)-utf8.UTFMax; i-- {
		if utf8.RuneStart(b[i]) {
			if utf8.FullRune(b[i:]) {
				return len(b)
			}
			return i
		}
	}
	return len(b)
}
