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

// ExtractTar safely extracts regular files and directories from a tar stream into an empty
// or non-existent destDir using DefaultExtractLimits.
func ExtractTar(r io.Reader, destDir string) error {
	return ExtractTarWithLimits(r, destDir, DefaultExtractLimits())
}

// ExtractTarWithLimits safely extracts regular files and directories from a tar stream into destDir.
// It rejects non-empty or symlink destination directories, path traversal entries, symlinks/device nodes,
// pre-existing symlinks (via os.OpenRoot + O_EXCL), and archives exceeding size or entry caps.
func ExtractTarWithLimits(r io.Reader, destDir string, limits ExtractLimits) error {
	absDest, err := filepath.Abs(destDir)
	if err != nil {
		return fmt.Errorf("resolve dest dir %q: %w", destDir, err)
	}
	if err := ensureCleanExtractDest(absDest); err != nil {
		return err
	}

	root, err := os.OpenRoot(absDest)
	if err != nil {
		return fmt.Errorf("open root %q: %w", absDest, err)
	}
	defer root.Close()

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

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := mkdirAllInRoot(root, cleanName, 0o755); err != nil {
				return fmt.Errorf("create dir %q: %w", cleanName, err)
			}
		case tar.TypeReg, tar.TypeRegA:
			if limits.MaxEntryBytes > 0 && hdr.Size > limits.MaxEntryBytes {
				return fmt.Errorf("tar entry %q size %d exceeds max entry limit (%d)", hdr.Name, hdr.Size, limits.MaxEntryBytes)
			}
			if limits.MaxTotalBytes > 0 && totalBytes+hdr.Size > limits.MaxTotalBytes {
				return fmt.Errorf("tar archive exceeds max total size (%d)", limits.MaxTotalBytes)
			}

			if parent := filepath.Dir(cleanName); parent != "." && parent != "" {
				if err := mkdirAllInRoot(root, parent, 0o755); err != nil {
					return fmt.Errorf("create parent dir for %q: %w", cleanName, err)
				}
			}
			perm := os.FileMode(hdr.Mode).Perm()
			if perm == 0 {
				perm = 0o644
			}
			// O_EXCL inside os.Root guarantees we never follow or truncate a pre-existing symlink or duplicate entry.
			out, err := root.OpenFile(cleanName, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm)
			if err != nil {
				return fmt.Errorf("create %q: %w", cleanName, err)
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
				_ = root.Remove(cleanName)
				return fmt.Errorf("extract %q: %w", cleanName, copyErr)
			}
			if closeErr != nil {
				return fmt.Errorf("close %q: %w", cleanName, closeErr)
			}
			if maxAllowed >= 0 && written > maxAllowed {
				_ = root.Remove(cleanName)
				return fmt.Errorf("tar entry %q exceeded size limit (%d bytes)", hdr.Name, maxAllowed)
			}
			totalBytes += written
		default:
			return fmt.Errorf("unsupported tar entry type %q for %q", hdr.Typeflag, hdr.Name)
		}
	}
}

func ensureCleanExtractDest(absDest string) error {
	info, err := os.Lstat(absDest)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if mkErr := os.MkdirAll(absDest, 0o755); mkErr != nil {
				return fmt.Errorf("create dest dir %q: %w", absDest, mkErr)
			}
			return nil
		}
		return fmt.Errorf("stat dest dir %q: %w", absDest, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("dest dir %q must not be a symlink", absDest)
	}
	if !info.IsDir() {
		return fmt.Errorf("dest path %q exists and is not a directory", absDest)
	}
	existing, err := os.ReadDir(absDest)
	if err != nil {
		return fmt.Errorf("read dest dir %q: %w", absDest, err)
	}
	if len(existing) > 0 {
		return fmt.Errorf("dest dir %q is not empty; specify a fresh --output directory to avoid mixing previous run artifacts", absDest)
	}
	return nil
}

func mkdirAllInRoot(root *os.Root, relDir string, perm os.FileMode) error {
	parts := strings.Split(filepath.Clean(relDir), string(filepath.Separator))
	cur := ""
	for _, p := range parts {
		if p == "" || p == "." {
			continue
		}
		if cur == "" {
			cur = p
		} else {
			cur = filepath.Join(cur, p)
		}
		if err := root.Mkdir(cur, perm); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
	}
	return nil
}
