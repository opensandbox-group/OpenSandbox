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
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// WriteSingleFileTar streams a single local file into w as a tar archive entry named entryName.
func WriteSingleFileTar(w io.Writer, srcPath, entryName string, mode int64) error {
	f, err := os.Open(srcPath)
	if err != nil {
		return fmt.Errorf("open %q: %w", srcPath, err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat %q: %w", srcPath, err)
	}
	if mode == 0 {
		mode = int64(info.Mode().Perm())
	}

	tw := tar.NewWriter(w)
	hdr := &tar.Header{
		Name:    path.Clean(strings.TrimPrefix(entryName, "/")),
		Mode:    mode,
		Size:    info.Size(),
		ModTime: info.ModTime(),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("write tar header for %q: %w", entryName, err)
	}
	if _, err := io.Copy(tw, f); err != nil {
		return fmt.Errorf("copy %q into tar stream: %w", srcPath, err)
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("close tar writer: %w", err)
	}
	return nil
}

// WriteBytesTar streams in-memory bytes into w as a single-file tar archive entry.
func WriteBytesTar(w io.Writer, entryName string, data []byte, mode int64) error {
	if mode == 0 {
		mode = 0o644
	}
	tw := tar.NewWriter(w)
	hdr := &tar.Header{
		Name: path.Clean(strings.TrimPrefix(entryName, "/")),
		Mode: mode,
		Size: int64(len(data)),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("write tar header for %q: %w", entryName, err)
	}
	if _, err := tw.Write(data); err != nil {
		return fmt.Errorf("write tar payload for %q: %w", entryName, err)
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("close tar writer: %w", err)
	}
	return nil
}

// ExtractTar safely extracts regular files and directories from a tar stream into destDir,
// rejecting path traversal entries and symlinks/device nodes.
func ExtractTar(r io.Reader, destDir string) error {
	absDest, err := filepath.Abs(destDir)
	if err != nil {
		return fmt.Errorf("resolve dest dir %q: %w", destDir, err)
	}
	if err := os.MkdirAll(absDest, 0o755); err != nil {
		return fmt.Errorf("create dest dir %q: %w", absDest, err)
	}

	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read tar entry: %w", err)
		}

		cleanName := filepath.Clean(hdr.Name)
		if cleanName == "." || cleanName == "" {
			continue
		}
		if filepath.IsAbs(cleanName) || cleanName == ".." || strings.HasPrefix(cleanName, ".."+string(filepath.Separator)) {
			return fmt.Errorf("unsafe tar entry path %q", hdr.Name)
		}

		targetPath := filepath.Join(absDest, cleanName)
		rel, err := filepath.Rel(absDest, targetPath)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("tar entry %q escapes destination %q", hdr.Name, absDest)
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(targetPath, 0o755); err != nil {
				return fmt.Errorf("create dir %q: %w", targetPath, err)
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
				return fmt.Errorf("create parent dir for %q: %w", targetPath, err)
			}
			perm := os.FileMode(hdr.Mode).Perm()
			if perm == 0 {
				perm = 0o644
			}
			out, err := os.OpenFile(targetPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
			if err != nil {
				return fmt.Errorf("open %q: %w", targetPath, err)
			}
			if _, err := io.Copy(out, tr); err != nil {
				_ = out.Close()
				return fmt.Errorf("extract %q: %w", targetPath, err)
			}
			if err := out.Close(); err != nil {
				return fmt.Errorf("close %q: %w", targetPath, err)
			}
		default:
			return fmt.Errorf("unsupported tar entry type %q for %q", hdr.Typeflag, hdr.Name)
		}
	}
}
