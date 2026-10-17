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

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"unicode/utf8"
)

// maxOutputLineBytes bounds how much of one output line is buffered. A longer
// line is delivered in pieces instead of failing the read, so the pipe keeps
// draining and the markers that follow are still seen.
const maxOutputLineBytes = 16 * 1024 * 1024

// scanOutputLines calls onLine for each line read from r, without the line
// terminator, as bufio.ScanLines does. A line longer than maxOutputLineBytes
// is passed in pieces with complete=false for all but the last piece; pieces
// never split a UTF-8 sequence. Scanning stops when onLine returns false or at
// EOF, and any read error other than io.EOF is returned.
func scanOutputLines(r io.Reader, onLine func(text string, complete bool) bool) error {
	reader := bufio.NewReaderSize(r, 64*1024)
	// line accumulates a line that does not fit in the reader's buffer.
	var line []byte
	for {
		chunk, err := reader.ReadSlice('\n')
		switch {
		case err == nil:
			if len(line) > 0 {
				line = append(line, chunk...)
				chunk, line = line, line[:0]
			}
			if !onLine(string(dropLineEnd(chunk)), true) {
				return nil
			}
		case errors.Is(err, bufio.ErrBufferFull):
			line = append(line, chunk...)
			if len(line) < maxOutputLineBytes {
				continue
			}
			n := lastRuneBoundary(line)
			if !onLine(string(line[:n]), false) {
				return nil
			}
			line = append(line[:0], line[n:]...)
		default:
			line = append(line, chunk...)
			if len(line) > 0 {
				onLine(string(dropLineEnd(line)), true)
			}
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

// dropLineEnd strips a trailing "\n", "\r\n" or, at EOF, "\r".
func dropLineEnd(line []byte) []byte {
	line = bytes.TrimSuffix(line, []byte("\n"))
	return bytes.TrimSuffix(line, []byte("\r"))
}

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
