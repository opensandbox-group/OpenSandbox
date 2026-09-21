// Copyright 2025 The OpenSandbox Authors
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

package glob

// isValidPattern checks whether a glob pattern is well-formed.
//
//nolint:gocognit
func isValidPattern(s string, separator rune) bool {
	altDepth := 0
	l := len(s)
VALIDATE:
	for i := 0; i < l; i++ {
		switch s[i] {
		case '\\':
			if separator != '\\' {
				if i++; i >= l {
					return false
				}
			}
			continue

		case '[':
			if i++; i >= l {
				return false
			}
			if s[i] == '^' || s[i] == '!' {
				i++
			}
			if i >= l || s[i] == ']' {
				return false
			}

			for ; i < l; i++ {
				if separator != '\\' && s[i] == '\\' {
					i++
				} else if s[i] == ']' {
					continue VALIDATE
				}
			}

			return false

		case '{':
			altDepth++
			continue

		case '}':
			if altDepth == 0 {
				return false
			}
			altDepth--
			continue
		}
	}

	return altDepth == 0
}
