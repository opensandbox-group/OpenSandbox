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

//go:build !windows

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
	long := strings.Repeat("a", maxScanLineBytes+100*1024)
	pieces := collectOutputLines(t, long+"\nnext\n")
	require.Equal(t, []outputPiece{
		{long[:maxScanLineBytes], false},
		{long[maxScanLineBytes:], true},
		{"next", true},
	}, pieces)
}

func TestScanOutputLines_LineOfExactlyMaxBytes(t *testing.T) {
	long := strings.Repeat("a", maxScanLineBytes)
	pieces := collectOutputLines(t, long+"\nnext\n")
	require.Equal(t, []outputPiece{
		{long, true},
		{"next", true},
	}, pieces)
}

func TestScanOutputLines_CRLFAtPieceBoundary(t *testing.T) {
	long := strings.Repeat("a", maxScanLineBytes-1)
	pieces := collectOutputLines(t, long+"\r\nnext\n")
	require.Equal(t, []outputPiece{
		{long, true},
		{"next", true},
	}, pieces)
}

func TestScanOutputLines_DoesNotSplitRune(t *testing.T) {
	// The odd prefix puts the piece boundary inside a two-byte rune.
	long := "a" + strings.Repeat("é", maxScanLineBytes/2+64*1024)
	pieces := collectOutputLines(t, long)
	require.Len(t, pieces, 2)
	require.False(t, pieces[0].complete)
	require.Len(t, pieces[0].text, maxScanLineBytes-1)
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

func TestHoldBackMarkerPrefix(t *testing.T) {
	cases := []struct {
		piece, marker, send, held string
	}{
		{"abcdef", "XYZ", "abcd", "ef"},
		{"abcdef", "X", "abcdef", ""},
		{"ab", "WXYZ", "", "ab"},
		// The cut moves back so the two-byte "é" is not split.
		{"abcé", "XY", "abc", "é"},
	}
	for _, tc := range cases {
		send, held := holdBackMarkerPrefix(tc.piece, tc.marker)
		require.Equal(t, tc.send, send, "send for %q, %q", tc.piece, tc.marker)
		require.Equal(t, tc.held, held, "held for %q, %q", tc.piece, tc.marker)
	}
}
