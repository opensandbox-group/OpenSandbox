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

func findUnescapedByteIndex(s string, c byte, allowEscaping bool) int {
	l := len(s)
	for i := 0; i < l; i++ {
		if allowEscaping && s[i] == '\\' {
			// skip next byte
			i++
		} else if s[i] == c {
			return i
		}
	}
	return -1
}

// findMatchedClosingAltIndex finds the matching `}` for a `{`.
func findMatchedClosingAltIndex(s string, allowEscaping bool) int {
	return findMatchedClosingSymbolsIndex(s, allowEscaping, '{', '}', 1)
}

// findMatchedClosingBracketIndex finds the matching `)` for a `(`.
func findMatchedClosingBracketIndex(s string, allowEscaping bool) int {
	return findMatchedClosingSymbolsIndex(s, allowEscaping, '(', ')', 0)
}

// findNextCommaIndex returns the next comma outside nested braces.
func findNextCommaIndex(s string, allowEscaping bool) int {
	alts := 1
	l := len(s)
	for i := 0; i < l; i++ {
		if allowEscaping && s[i] == '\\' {
			i++
		} else if s[i] == '{' {
			alts++
		} else if s[i] == '}' {
			alts--
		} else if s[i] == ',' && alts == 1 {
			return i
		}
	}
	return -1
}

func findMatchedClosingSymbolsIndex(s string, allowEscaping bool, left, right uint8, begin int) int {
	l := len(s)
	for i := 0; i < l; i++ {
		if allowEscaping && s[i] == '\\' {
			i++
		} else if s[i] == left {
			begin++
		} else if s[i] == right {
			if begin--; begin == 0 {
				return i
			}
		}
	}
	return -1
}
