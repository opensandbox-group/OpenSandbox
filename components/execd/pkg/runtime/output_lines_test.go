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
	"strings"
	"testing"
	"testing/iotest"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

type outputPiece struct {
	text     string
	complete bool
}

func collectOutputLines(t *testing.T, input string) []outputPiece {
	t.Helper()
	var pieces []outputPiece
	err := scanOutputLines(strings.NewReader(input), func(text string, complete bool) bool {
		pieces = append(pieces, outputPiece{text, complete})
		return true
	})
	require.NoError(t, err)
	return pieces
}

func TestScanOutputLines_MatchesScanLines(t *testing.T) {
	pieces := collectOutputLines(t, "a\r\n\nb\nlast\r")
	require.Equal(t, []outputPiece{
		{"a", true},
		{"", true},
		{"b", true},
		{"last", true},
	}, pieces)
}

func TestScanOutputLines_LineLongerThanReaderBuffer(t *testing.T) {
	long := strings.Repeat("a", 200*1024)
	pieces := collectOutputLines(t, long+"\r\nnext\n")
	require.Equal(t, []outputPiece{
		{long, true},
		{"next", true},
	}, pieces)
}

func TestScanOutputLines_SplitsLongLine(t *testing.T) {
	long := strings.Repeat("a", maxOutputLineBytes+10)
	pieces := collectOutputLines(t, long+"\nnext\n")
	require.Len(t, pieces, 3)
	require.Equal(t, outputPiece{long[:maxOutputLineBytes], false}, pieces[0])
	require.Equal(t, outputPiece{long[maxOutputLineBytes:], true}, pieces[1])
	require.Equal(t, outputPiece{"next", true}, pieces[2])
}

func TestScanOutputLines_DoesNotSplitRune(t *testing.T) {
	// The odd prefix puts the piece boundary inside a two-byte rune.
	long := "a" + strings.Repeat("é", maxOutputLineBytes/2)
	pieces := collectOutputLines(t, long)
	require.Len(t, pieces, 2)
	require.False(t, pieces[0].complete)
	require.Len(t, pieces[0].text, maxOutputLineBytes-1)
	for _, piece := range pieces {
		require.True(t, utf8.ValidString(piece.text))
	}
	require.Equal(t, long, pieces[0].text+pieces[1].text)
}

func TestScanOutputLines_StopsWhenCallbackReturnsFalse(t *testing.T) {
	var lines []string
	err := scanOutputLines(strings.NewReader("a\nstop\nb\n"), func(text string, _ bool) bool {
		lines = append(lines, text)
		return text != "stop"
	})
	require.NoError(t, err)
	require.Equal(t, []string{"a", "stop"}, lines)
}

func TestScanOutputLines_ReturnsReadError(t *testing.T) {
	var lines []string
	err := scanOutputLines(iotest.TimeoutReader(strings.NewReader("a\nb")), func(text string, _ bool) bool {
		lines = append(lines, text)
		return true
	})
	require.ErrorIs(t, err, iotest.ErrTimeout)
	require.Equal(t, []string{"a", "b"}, lines)
}
