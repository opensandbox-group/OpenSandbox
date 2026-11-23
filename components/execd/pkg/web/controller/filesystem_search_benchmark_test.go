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

package controller

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/alibaba/opensandbox/execd/pkg/web/model"
)

func BenchmarkFilesystemControllerSearchFiles(b *testing.B) {
	for _, fileCount := range []int{10_000, 100_000} {
		b.Run(fmt.Sprintf("Files%d", fileCount), func(b *testing.B) {
			searchRoot := createSearchBenchmarkTree(b, fileCount)
			for _, scenario := range []struct {
				name    string
				pattern string
				matches int
			}{
				{name: "Match0Pct", pattern: "absent-*.txt", matches: 0},
				{name: "Match1Pct", pattern: "match-*.txt", matches: fileCount / 100},
				{name: "Match100Pct", pattern: "*.txt", matches: fileCount},
			} {
				b.Run(scenario.name, func(b *testing.B) {
					rawURL := "/files/search?" + url.Values{
						"path":    {searchRoot},
						"pattern": {scenario.pattern},
					}.Encode()
					var responseBody []byte
					b.ReportAllocs()
					b.ResetTimer()
					for iteration := 0; iteration < b.N; iteration++ {
						ctx, response := newTestContext(http.MethodGet, rawURL, nil)
						NewFilesystemController(ctx).SearchFiles()
						if response.Code != http.StatusOK {
							b.Fatalf("search returned HTTP %d: %s", response.Code, response.Body.String())
						}
						responseBody = response.Body.Bytes()
					}
					b.StopTimer()

					var files []model.FileInfo
					if err := json.Unmarshal(responseBody, &files); err != nil {
						b.Fatal(err)
					}
					if len(files) != scenario.matches {
						b.Fatalf("search returned %d files, want %d", len(files), scenario.matches)
					}
				})
			}
		})
	}
}

func createSearchBenchmarkTree(b *testing.B, fileCount int) string {
	b.Helper()
	const filesPerDirectory = 1_000
	fixtureRoot := b.TempDir()
	searchRoot := filepath.Join(fixtureRoot, "tree")
	for directoryIndex := 0; directoryIndex < fileCount/filesPerDirectory; directoryIndex++ {
		directoryPath := filepath.Join(searchRoot, fmt.Sprintf("group-%02d", directoryIndex/10), fmt.Sprintf("dir-%03d", directoryIndex))
		if err := os.MkdirAll(directoryPath, 0o755); err != nil {
			b.Fatal(err)
		}

		// Hard links keep setup cheap while retaining distinct directory entries.
		// Per-directory seeds stay below hard-link limits; shared inodes model
		// warm metadata rather than a cold inode cache for every file.
		seedPath := filepath.Join(fixtureRoot, fmt.Sprintf("seed-%03d", directoryIndex))
		if err := os.WriteFile(seedPath, []byte("search benchmark payload\n"), 0o644); err != nil {
			b.Fatal(err)
		}
		for fileIndex := 0; fileIndex < filesPerDirectory; fileIndex++ {
			prefix := "file"
			if fileIndex%100 == 0 {
				prefix = "match"
			}
			filePath := filepath.Join(directoryPath, fmt.Sprintf("%s-%04d.txt", prefix, fileIndex))
			if err := os.Link(seedPath, filePath); err != nil {
				b.Fatal(err)
			}
		}
	}
	return searchRoot
}
