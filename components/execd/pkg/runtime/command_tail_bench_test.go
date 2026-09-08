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
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// One operation is an idle poll after a 1 MiB unterminated line was primed.
func BenchmarkCommandOutputIdlePollAfterStatic1MiB(b *testing.B) {
	path := filepath.Join(b.TempDir(), "stdout.log")
	content := bytes.Repeat([]byte{'x'}, 1<<20)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		b.Fatal(err)
	}

	var tail commandOutputTail
	tail.read(path, func(string) {
		b.Fatal("unterminated output emitted without a flush")
	}, false)
	if tail.offset != int64(len(content)) {
		b.Fatalf("prime position = %d, want %d", tail.offset, len(content))
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tail.read(path, func(string) {
			b.Fatal("unterminated output emitted without a flush")
		}, false)
	}
	b.StopTimer()
	if tail.offset != int64(len(content)) {
		b.Fatalf("final position = %d, want %d", tail.offset, len(content))
	}
}

// One operation is 16 polls after 64 KiB appends, followed by a terminating
// newline and one final poll. File writes are excluded from ns/op.
func BenchmarkCommandOutputGrowing1MiBLine(b *testing.B) {
	path := filepath.Join(b.TempDir(), "stdout.log")
	file, err := os.Create(path)
	if err != nil {
		b.Fatal(err)
	}
	defer file.Close()

	const (
		appendCount = 16
		appendSize  = 64 << 10
	)
	chunk := bytes.Repeat([]byte{'x'}, appendSize)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		if err := file.Truncate(0); err != nil {
			b.Fatal(err)
		}
		if _, err := file.Seek(0, 0); err != nil {
			b.Fatal(err)
		}
		var tail commandOutputTail
		lines, outputBytes := 0, 0

		for j := 0; j < appendCount; j++ {
			if _, err := file.Write(chunk); err != nil {
				b.Fatal(err)
			}
			b.StartTimer()
			tail.read(path, func(s string) {
				lines++
				outputBytes += len(s)
			}, false)
			b.StopTimer()
		}
		if _, err := file.Write([]byte{'\n'}); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		tail.read(path, func(s string) {
			lines++
			outputBytes += len(s)
		}, false)
		b.StopTimer()

		if want := int64(appendCount*appendSize + 1); tail.offset != want {
			b.Fatalf("final position = %d, want %d", tail.offset, want)
		}
		if lines != 1 || outputBytes != appendCount*appendSize {
			b.Fatalf("output = %d lines/%d bytes, want 1 line/%d bytes", lines, outputBytes, appendCount*appendSize)
		}
	}
}

// One operation reads a static batch of 100 ordinary short lines.
func BenchmarkCommandOutputShortLines100(b *testing.B) {
	path := filepath.Join(b.TempDir(), "stdout.log")
	const line = "short output line"
	content := bytes.Repeat([]byte(line+"\n"), 100)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		b.Fatal(err)
	}

	var pos int64
	var lines, outputBytes int

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		lines, outputBytes = 0, 0
		var tail commandOutputTail
		tail.read(path, func(s string) {
			lines++
			outputBytes += len(s)
		}, false)
		pos = tail.offset
	}
	b.StopTimer()
	if pos != int64(len(content)) {
		b.Fatalf("final position = %d, want %d", pos, len(content))
	}
	if lines != 100 || outputBytes != 100*len(line) {
		b.Fatalf("output = %d lines/%d bytes, want 100 lines/%d bytes", lines, outputBytes, 100*len(line))
	}
}
