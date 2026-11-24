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

package runner

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWriteSingleFileAndExtractTarRoundTrip(t *testing.T) {
	srcDir := t.TempDir()
	srcFile := filepath.Join(srcDir, "worker-bin")
	payload := []byte("#!/bin/sh\necho hello\n")
	require.NoError(t, os.WriteFile(srcFile, payload, 0o600))

	var buf bytes.Buffer
	require.NoError(t, WriteSingleFileTar(&buf, srcFile, "opensandbox-bench", 0o755))

	destDir := t.TempDir()
	require.NoError(t, ExtractTar(&buf, destDir))

	extractedPath := filepath.Join(destDir, "opensandbox-bench")
	got, err := os.ReadFile(extractedPath)
	require.NoError(t, err)
	require.Equal(t, payload, got)

	info, err := os.Stat(extractedPath)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o755), info.Mode().Perm())
}

func TestWriteBytesTarAndExtractNestedFiles(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	require.NoError(t, tw.WriteHeader(&tar.Header{
		Name:     "traces/",
		Typeflag: tar.TypeDir,
		Mode:     0o755,
	}))
	traceData := []byte(`{"trace_id":"abc"}`)
	require.NoError(t, tw.WriteHeader(&tar.Header{
		Name:     "traces/abc.json",
		Typeflag: tar.TypeReg,
		Mode:     0o644,
		Size:     int64(len(traceData)),
	}))
	_, err := tw.Write(traceData)
	require.NoError(t, err)
	require.NoError(t, tw.Close())

	destDir := t.TempDir()
	require.NoError(t, ExtractTar(&buf, destDir))

	got, err := os.ReadFile(filepath.Join(destDir, "traces", "abc.json"))
	require.NoError(t, err)
	require.Equal(t, traceData, got)
}

func TestExtractTarRejectsPathTraversalAndSymlinks(t *testing.T) {
	t.Run("path traversal", func(t *testing.T) {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		require.NoError(t, tw.WriteHeader(&tar.Header{
			Name:     "../escape.txt",
			Typeflag: tar.TypeReg,
			Mode:     0o644,
			Size:     4,
		}))
		_, err := tw.Write([]byte("evil"))
		require.NoError(t, err)
		require.NoError(t, tw.Close())

		err = ExtractTar(&buf, t.TempDir())
		require.Error(t, err)
		require.Contains(t, err.Error(), "unsafe tar entry")
	})

	t.Run("symlink rejected", func(t *testing.T) {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		require.NoError(t, tw.WriteHeader(&tar.Header{
			Name:     "link",
			Typeflag: tar.TypeSymlink,
			Linkname: "/etc/passwd",
		}))
		require.NoError(t, tw.Close())

		err := ExtractTar(&buf, t.TempDir())
		require.Error(t, err)
		require.Contains(t, err.Error(), "unsupported tar entry type")
	})
}

func TestExtractTarEnforcesLimits(t *testing.T) {
	makeArchive := func(files map[string][]byte) *bytes.Buffer {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		for name, data := range files {
			require.NoError(t, tw.WriteHeader(&tar.Header{
				Name:     name,
				Typeflag: tar.TypeReg,
				Mode:     0o644,
				Size:     int64(len(data)),
			}))
			_, err := tw.Write(data)
			require.NoError(t, err)
		}
		require.NoError(t, tw.Close())
		return &buf
	}

	t.Run("max entry size exceeded", func(t *testing.T) {
		buf := makeArchive(map[string][]byte{"big.txt": []byte("0123456789")})
		err := ExtractTarWithLimits(buf, t.TempDir(), ExtractLimits{MaxEntries: 10, MaxEntryBytes: 5, MaxTotalBytes: 100})
		require.Error(t, err)
		require.Contains(t, err.Error(), "exceeds max entry limit")
	})

	t.Run("max total size exceeded", func(t *testing.T) {
		buf := makeArchive(map[string][]byte{"a.txt": []byte("12345"), "b.txt": []byte("67890")})
		err := ExtractTarWithLimits(buf, t.TempDir(), ExtractLimits{MaxEntries: 10, MaxEntryBytes: 10, MaxTotalBytes: 8})
		require.Error(t, err)
		require.Contains(t, err.Error(), "exceeds max total size")
	})

	t.Run("max entry count exceeded", func(t *testing.T) {
		buf := makeArchive(map[string][]byte{"a.txt": []byte("1"), "b.txt": []byte("2")})
		err := ExtractTarWithLimits(buf, t.TempDir(), ExtractLimits{MaxEntries: 1, MaxEntryBytes: 100, MaxTotalBytes: 100})
		require.Error(t, err)
		require.Contains(t, err.Error(), "exceeds max entry count")
	})
}

func TestExtractTarRejectsNonEmptyOrSymlinkDest(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, WriteBytesTar(&buf, "report.json", []byte(`{}`), 0o644))

	t.Run("non-empty destination rejected", func(t *testing.T) {
		dest := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dest, "stale.json"), []byte("old"), 0o644))
		err := ExtractTar(bytes.NewReader(buf.Bytes()), dest)
		require.Error(t, err)
		require.Contains(t, err.Error(), "not empty")
	})

	t.Run("symlink destination rejected", func(t *testing.T) {
		parent := t.TempDir()
		realDir := filepath.Join(parent, "real")
		require.NoError(t, os.MkdirAll(realDir, 0o755))
		linkDir := filepath.Join(parent, "link")
		require.NoError(t, os.Symlink(realDir, linkDir))

		err := ExtractTar(bytes.NewReader(buf.Bytes()), linkDir)
		require.Error(t, err)
		require.Contains(t, err.Error(), "must not be a symlink")
	})
}
