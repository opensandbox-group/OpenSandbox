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
//
// This code is based on or derived from doublestar
// Copyright (c) 2014 Bob Matcuk
// Licensed under MIT License
// https://github.com/bmatcuk/doublestar/blob/master/LICENSE

package glob

import (
	"path/filepath"
	"unicode/utf8"

	globutil "github.com/bmatcuk/doublestar/v4"
)

// PathMatch is filepath.Match compatible but honors doublestar semantics.
func PathMatch(pattern, name string) (bool, error) {
	return matchWithSeparator(pattern, name, filepath.Separator, true)
}

func matchWithSeparator(pattern, name string, separator rune, validate bool) (matched bool, err error) {
	return doMatchWithSeparator(pattern, name, separator, validate, -1, -1, -1, -1, 0, 0)
}

//nolint:gocognit,nestif,gocyclo,maintidx
func doMatchWithSeparator(pattern, name string, separator rune, validate bool, doublestarPatternBacktrack, doublestarNameBacktrack, starPatternBacktrack, starNameBacktrack, patIdx, nameIdx int) (matched bool, err error) {
	patLen := len(pattern)
	nameLen := len(name)
	startOfSegment := true
MATCH:
	for nameIdx < nameLen {
		if patIdx < patLen {
			switch pattern[patIdx] {
			case '*':
				if patIdx++; patIdx < patLen && pattern[patIdx] == '*' {
					// doublestar - must begin with a path separator, otherwise we'll
					patIdx++
					if startOfSegment {
						if patIdx >= patLen {
							// pattern ends in `/**`: return true
							return true, nil
						}

						// doublestar must also end with a path separator, otherwise we're
						patRune, patRuneLen := utf8.DecodeRuneInString(pattern[patIdx:])
						if patRune == separator {
							patIdx += patRuneLen

							doublestarPatternBacktrack = patIdx
							doublestarNameBacktrack = nameIdx
							starPatternBacktrack = -1
							starNameBacktrack = -1
							continue
						}
					}
				}
				startOfSegment = false

				starPatternBacktrack = patIdx
				starNameBacktrack = nameIdx
				continue

			case '?':
				startOfSegment = false
				nameRune, nameRuneLen := utf8.DecodeRuneInString(name[nameIdx:])
				if nameRune == separator {
					// `?` cannot match the separator
					break
				}

				patIdx++
				nameIdx += nameRuneLen
				continue

			case '[':
				startOfSegment = false
				if patIdx++; patIdx >= patLen {
					// class didn't end
					return false, globutil.ErrBadPattern
				}
				nameRune, nameRuneLen := utf8.DecodeRuneInString(name[nameIdx:])

				matched := false
				negate := pattern[patIdx] == '!' || pattern[patIdx] == '^'
				if negate {
					patIdx++
				}

				if patIdx >= patLen || pattern[patIdx] == ']' {
					// class didn't end or empty character class
					return false, globutil.ErrBadPattern
				}

				last := utf8.MaxRune
				for patIdx < patLen && pattern[patIdx] != ']' {
					patRune, patRuneLen := utf8.DecodeRuneInString(pattern[patIdx:])
					patIdx += patRuneLen

					// match a range
					if last < utf8.MaxRune && patRune == '-' && patIdx < patLen && pattern[patIdx] != ']' {
						if pattern[patIdx] == '\\' {
							// next character is escaped
							patIdx++
						}
						patRune, patRuneLen = utf8.DecodeRuneInString(pattern[patIdx:])
						patIdx += patRuneLen

						if last <= nameRune && nameRune <= patRune {
							matched = true
							break
						}

						// didn't match range - reset `last`
						last = utf8.MaxRune
						continue
					}

					// not a range - check if the next rune is escaped
					if patRune == '\\' {
						patRune, patRuneLen = utf8.DecodeRuneInString(pattern[patIdx:])
						patIdx += patRuneLen
					}

					// check if the rune matches
					if patRune == nameRune {
						matched = true
						break
					}

					// no matches yet
					last = patRune
				}

				if matched == negate {
					// failed to match - if we reached the end of the pattern, that means
					if patIdx >= patLen {
						return false, globutil.ErrBadPattern
					}
					break
				}

				closingIdx := findUnescapedByteIndex(pattern[patIdx:], ']', true)
				if closingIdx == -1 {
					// no closing `]`
					return false, globutil.ErrBadPattern
				}

				patIdx += closingIdx + 1
				nameIdx += nameRuneLen
				continue
			case '!':
				negateIdx := patIdx
				// begin index of (
				patIdx++
				closingIdx := findMatchedClosingBracketIndex(pattern[patIdx:], separator != '\\')
				if closingIdx == -1 {
					return false, globutil.ErrBadPattern
				}
				closingIdx += patIdx

				result, err := doMatchWithSeparator(pattern[:negateIdx]+pattern[patIdx+1:closingIdx]+pattern[closingIdx+1:], name, separator, validate, doublestarPatternBacktrack, doublestarNameBacktrack, starPatternBacktrack, starNameBacktrack, negateIdx, nameIdx)
				if err != nil {
					return false, err
				} else if !result {
					return true, nil
				} else {
					return false, nil
				}
			case '{':
				startOfSegment = false //nolint:ineffassign
				beforeIdx := patIdx
				patIdx++
				closingIdx := findMatchedClosingAltIndex(pattern[patIdx:], separator != '\\')
				if closingIdx == -1 {
					// no closing `}`
					return false, globutil.ErrBadPattern
				}
				closingIdx += patIdx

				for {
					commaIdx := findNextCommaIndex(pattern[patIdx:closingIdx], separator != '\\')
					if commaIdx == -1 {
						break
					}
					commaIdx += patIdx

					result, err := doMatchWithSeparator(pattern[:beforeIdx]+pattern[patIdx:commaIdx]+pattern[closingIdx+1:], name, separator, validate, doublestarPatternBacktrack, doublestarNameBacktrack, starPatternBacktrack, starNameBacktrack, beforeIdx, nameIdx)
					if result || err != nil {
						return result, err
					}

					patIdx = commaIdx + 1
				}
				return doMatchWithSeparator(pattern[:beforeIdx]+pattern[patIdx:closingIdx]+pattern[closingIdx+1:], name, separator, validate, doublestarPatternBacktrack, doublestarNameBacktrack, starPatternBacktrack, starNameBacktrack, beforeIdx, nameIdx)

			case '\\':
				if separator != '\\' {
					// next rune is "escaped" in the pattern - literal match
					if patIdx++; patIdx >= patLen {
						// pattern ended
						return false, globutil.ErrBadPattern
					}
				}
				fallthrough

			default:
				patRune, patRuneLen := utf8.DecodeRuneInString(pattern[patIdx:])
				nameRune, nameRuneLen := utf8.DecodeRuneInString(name[nameIdx:])
				if patRune != nameRune {
					if separator != '\\' && patIdx > 0 && pattern[patIdx-1] == '\\' {
						// if this rune was meant to be escaped, we need to move patIdx
						patIdx--
					}
					break
				}

				patIdx += patRuneLen
				nameIdx += nameRuneLen
				startOfSegment = patRune == separator
				continue
			}
		}

		if starPatternBacktrack >= 0 {
			// `*` backtrack, but only if the `name` rune isn't the separator
			nameRune, nameRuneLen := utf8.DecodeRuneInString(name[starNameBacktrack:])
			if nameRune != separator {
				starNameBacktrack += nameRuneLen
				patIdx = starPatternBacktrack
				nameIdx = starNameBacktrack
				startOfSegment = false
				continue
			}
		}

		if doublestarPatternBacktrack >= 0 {
			// `**` backtrack, advance `name` past next separator
			nameIdx = doublestarNameBacktrack
			for nameIdx < nameLen {
				nameRune, nameRuneLen := utf8.DecodeRuneInString(name[nameIdx:])
				nameIdx += nameRuneLen
				if nameRune == separator {
					doublestarNameBacktrack = nameIdx
					patIdx = doublestarPatternBacktrack
					startOfSegment = true
					continue MATCH
				}
			}
		}

		if validate && patIdx < patLen && !isValidPattern(pattern[patIdx:], separator) {
			return false, globutil.ErrBadPattern
		}
		return false, nil
	}

	if nameIdx < nameLen {
		// we reached the end of `pattern` before the end of `name`
		return false, nil
	}

	// we've reached the end of `name`; we've successfully matched if we've also
	return isZeroLengthPattern(pattern[patIdx:], separator)
}

// nolint:nakedret
func isZeroLengthPattern(pattern string, separator rune) (ret bool, err error) {
	// `/**` is a special case - a pattern such as `path/to/a/**` *should* match
	if pattern == "" || pattern == "*" || pattern == "**" || pattern == string(separator)+"**" {
		return true, nil
	}

	if pattern[0] == '{' {
		closingIdx := findMatchedClosingAltIndex(pattern[1:], separator != '\\')
		if closingIdx == -1 {
			// no closing '}'
			return false, globutil.ErrBadPattern
		}
		closingIdx += 1

		patIdx := 1
		for {
			commaIdx := findNextCommaIndex(pattern[patIdx:closingIdx], separator != '\\')
			if commaIdx == -1 {
				break
			}
			commaIdx += patIdx

			ret, err = isZeroLengthPattern(pattern[patIdx:commaIdx]+pattern[closingIdx+1:], separator)
			if ret || err != nil {
				return
			}

			patIdx = commaIdx + 1
		}
		return isZeroLengthPattern(pattern[patIdx:closingIdx]+pattern[closingIdx+1:], separator)
	}

	// no luck - validate the rest of the pattern
	if !isValidPattern(pattern, separator) {
		return false, globutil.ErrBadPattern
	}
	return false, nil
}
