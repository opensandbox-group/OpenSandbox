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

package isolation

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func BenchmarkMergedViewSearch(b *testing.B) {
	for _, fileCount := range []int{10_000, 100_000} {
		b.Run(fmt.Sprintf("Files%d", fileCount), func(b *testing.B) {
			for _, layout := range []struct {
				name         string
				mode         WorkspaceMode
				upperPercent int
			}{
				{name: "RWSameRoot", mode: WorkspaceRW, upperPercent: 100},
				{name: "OverlayUpper1Pct", mode: WorkspaceOverlay, upperPercent: 1},
				{name: "OverlayUpper50Pct", mode: WorkspaceOverlay, upperPercent: 50},
				{name: "OverlayUpper90Pct", mode: WorkspaceOverlay, upperPercent: 90},
				{name: "ROLowerOnly", mode: WorkspaceRO},
			} {
				b.Run(layout.name, func(b *testing.B) {
					view := newMergedSearchBenchmarkView(b, fileCount, layout.mode, layout.upperPercent)
					for _, scenario := range []struct {
						name    string
						pattern string
						matches int
					}{
						{name: "Match0Pct", pattern: "absent-*.txt"},
						{name: "Match1Pct", pattern: "match-*.txt", matches: fileCount / 100},
						{name: "Match100Pct", pattern: "*.txt", matches: fileCount},
					} {
						b.Run(scenario.name, func(b *testing.B) {
							b.ReportAllocs()
							b.ResetTimer()
							var results []string
							for iteration := 0; iteration < b.N; iteration++ {
								var err error
								results, err = view.Search(".", scenario.pattern)
								if err != nil {
									b.Fatal(err)
								}
							}
							b.StopTimer()
							if len(results) != scenario.matches {
								b.Fatalf("search returned %d files, want %d", len(results), scenario.matches)
							}
						})
					}
				})
			}
		})
	}
}

func newMergedSearchBenchmarkView(b *testing.B, fileCount int, mode WorkspaceMode, upperPercent int) *MergedView {
	b.Helper()
	fixtureRoot := b.TempDir()
	lower := filepath.Join(fixtureRoot, "lower")
	upper := ""
	createMergedSearchBenchmarkLayer(b, fixtureRoot, lower, fileCount)
	if mode == WorkspaceRW {
		upper = lower
	} else if mode == WorkspaceOverlay {
		upper = filepath.Join(fixtureRoot, "upper")
		// Upper files replace identically named lower files, keeping the visible
		// result count fixed while varying the amount of upper traversal work.
		createMergedSearchBenchmarkLayer(b, fixtureRoot, upper, fileCount*upperPercent/100)
	}
	return NewMergedView(lower, upper, mode, uint32(os.Getuid()), uint32(os.Getgid()))
}

func createMergedSearchBenchmarkLayer(b *testing.B, fixtureRoot, layerRoot string, fileCount int) {
	b.Helper()
	const filesPerDirectory = 1_000
	for firstFile := 0; firstFile < fileCount; firstFile += filesPerDirectory {
		directory := filepath.Join(layerRoot, fmt.Sprintf("dir-%03d", firstFile/filesPerDirectory))
		if err := os.MkdirAll(directory, 0o755); err != nil {
			b.Fatal(err)
		}
		// Per-directory hard-link seeds keep setup cheap and model warm metadata.
		seed := filepath.Join(fixtureRoot, fmt.Sprintf("%s-seed-%03d", filepath.Base(layerRoot), firstFile/filesPerDirectory))
		if err := os.WriteFile(seed, nil, 0o644); err != nil {
			b.Fatal(err)
		}
		for fileIndex := firstFile; fileIndex < min(firstFile+filesPerDirectory, fileCount); fileIndex++ {
			prefix := "file"
			if fileIndex%100 == 0 {
				prefix = "match"
			}
			filePath := filepath.Join(directory, fmt.Sprintf("%s-%06d.txt", prefix, fileIndex))
			if err := os.Link(seed, filePath); err != nil {
				b.Fatal(err)
			}
		}
	}
}
