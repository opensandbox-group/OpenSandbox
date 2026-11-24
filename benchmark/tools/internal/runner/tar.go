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

// ExtractLimits bounds tar extraction against malformed or unexpectedly huge bundles.
type ExtractLimits struct {
	MaxEntries    int
	MaxEntryBytes int64
	MaxTotalBytes int64
}

func DefaultExtractLimits() ExtractLimits {
	return ExtractLimits{
		MaxEntries:    10000,
		MaxEntryBytes: 512 << 20, // 512 MiB per file
		MaxTotalBytes: 2 << 30,   // 2 GiB total
	}
}

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

// ExtractTar safely extracts regular files and directories from a tar stream into destDir
// using DefaultExtractLimits.
func ExtractTar(r io.Reader, destDir string) error {
	return ExtractTarWithLimits(r, destDir, DefaultExtractLimits())
}

// ExtractTarWithLimits safely extracts regular files and directories from a tar stream into destDir,
// rejecting path traversal entries, symlinks/device nodes, and archives exceeding size or entry caps.
func ExtractTarWithLimits(r io.Reader, destDir string, limits ExtractLimits) error {
	absDest, err := filepath.Abs(destDir)
	if err != nil {
		return fmt.Errorf("resolve dest dir %q: %w", destDir, err)
	}
	if err := os.MkdirAll(absDest, 0o755); err != nil {
		return fmt.Errorf("create dest dir %q: %w", absDest, err)
	}

	var (
		entries    int
		totalBytes int64
	)

	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read tar entry: %w", err)
		}

		entries++
		if limits.MaxEntries > 0 && entries > limits.MaxEntries {
			return fmt.Errorf("tar archive exceeds max entry count (%d)", limits.MaxEntries)
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
			if limits.MaxEntryBytes > 0 && hdr.Size > limits.MaxEntryBytes {
				return fmt.Errorf("tar entry %q size %d exceeds max entry limit (%d)", hdr.Name, hdr.Size, limits.MaxEntryBytes)
			}
			if limits.MaxTotalBytes > 0 && totalBytes+hdr.Size > limits.MaxTotalBytes {
				return fmt.Errorf("tar archive exceeds max total size (%d)", limits.MaxTotalBytes)
			}

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

			var src io.Reader = tr
			maxAllowed := int64(-1)
			if limits.MaxEntryBytes > 0 {
				maxAllowed = limits.MaxEntryBytes
			}
			if limits.MaxTotalBytes > 0 {
				rem := limits.MaxTotalBytes - totalBytes
				if maxAllowed < 0 || rem < maxAllowed {
					maxAllowed = rem
				}
			}
			if maxAllowed >= 0 {
				src = io.LimitReader(tr, maxAllowed+1)
			}

			written, copyErr := io.Copy(out, src)
			closeErr := out.Close()
			if copyErr != nil {
				_ = os.Remove(targetPath)
				return fmt.Errorf("extract %q: %w", targetPath, copyErr)
			}
			if closeErr != nil {
				return fmt.Errorf("close %q: %w", targetPath, closeErr)
			}
			if maxAllowed >= 0 && written > maxAllowed {
				_ = os.Remove(targetPath)
				return fmt.Errorf("tar entry %q exceeded size limit (%d bytes)", hdr.Name, maxAllowed)
			}
			totalBytes += written
		default:
			return fmt.Errorf("unsupported tar entry type %q for %q", hdr.Typeflag, hdr.Name)
		}
	}
}
