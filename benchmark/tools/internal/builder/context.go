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

package builder

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// VirtualFile is an extra file injected into the tar context without modifying the
// local source working tree (e.g. cross-compiled .build/linux-amd64/<bin> binaries).
type VirtualFile struct {
	ArchivePath string
	Data        []byte
	Mode        int64
}

// ArchiveRepoContext packs the required subset of a repository into a gzip-compressed
// tar archive for upload into the in-cluster BuildKit pod. Only paths relevant to the
// requested components are included, skipping heavy non-build directories (.git,
// benchmark, examples, sdks, console, node_modules, .venv, etc.).
func ArchiveRepoContext(repoDir string, kind RepoKind, extraFiles []VirtualFile) ([]byte, error) {
	absRoot, err := filepath.Abs(repoDir)
	if err != nil {
		return nil, fmt.Errorf("resolve repo dir %q: %w", repoDir, err)
	}
	st, err := os.Stat(absRoot)
	if err != nil || !st.IsDir() {
		return nil, fmt.Errorf("repo dir %q does not exist or is not a directory", repoDir)
	}

	roots := repoIncludeRoots(kind)

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	written := make(map[string]bool)
	extraSet := make(map[string]bool, len(extraFiles))
	for _, ef := range extraFiles {
		extraSet[filepath.ToSlash(strings.TrimLeft(ef.ArchivePath, "/"))] = true
	}

	writePath := func(relRoot string) error {
		fullStart := filepath.Join(absRoot, filepath.FromSlash(relRoot))
		if _, err := os.Lstat(fullStart); err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		return filepath.WalkDir(fullStart, func(curPath string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			rel, err := filepath.Rel(absRoot, curPath)
			if err != nil {
				return err
			}
			slashRel := filepath.ToSlash(rel)
			if slashRel == "." {
				return nil
			}
			if shouldSkipContextPath(slashRel, d.IsDir()) {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if extraSet[slashRel] {
				return nil
			}
			if written[slashRel] {
				return nil
			}

			info, err := d.Info()
			if err != nil {
				return err
			}

			if info.Mode()&os.ModeSymlink != 0 {
				linkTarget, err := os.Readlink(curPath)
				if err != nil {
					return err
				}
				hdr, err := tar.FileInfoHeader(info, linkTarget)
				if err != nil {
					return err
				}
				hdr.Name = slashRel
				written[slashRel] = true
				return tw.WriteHeader(hdr)
			}

			if !info.IsDir() && !info.Mode().IsRegular() {
				return nil
			}

			hdr, err := tar.FileInfoHeader(info, "")
			if err != nil {
				return err
			}
			hdr.Name = slashRel
			if info.IsDir() {
				hdr.Name += "/"
				written[slashRel] = true
				return tw.WriteHeader(hdr)
			}

			written[slashRel] = true
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
			f, err := os.Open(curPath)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(tw, f)
			closeErr := f.Close()
			if copyErr != nil {
				return copyErr
			}
			return closeErr
		})
	}

	for _, r := range roots {
		if err := writePath(r); err != nil {
			_ = tw.Close()
			_ = gz.Close()
			return nil, fmt.Errorf("archive %s/%s: %w", kind, r, err)
		}
	}

	sort.Slice(extraFiles, func(i, j int) bool {
		return extraFiles[i].ArchivePath < extraFiles[j].ArchivePath
	})
	for _, ef := range extraFiles {
		name := filepath.ToSlash(strings.TrimLeft(ef.ArchivePath, "/"))
		mode := ef.Mode
		if mode == 0 {
			mode = 0o755
		}
		hdr := &tar.Header{
			Name:     name,
			Mode:     mode,
			Size:     int64(len(ef.Data)),
			Typeflag: tar.TypeReg,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			_ = tw.Close()
			_ = gz.Close()
			return nil, fmt.Errorf("write virtual file %s header: %w", name, err)
		}
		if _, err := tw.Write(ef.Data); err != nil {
			_ = tw.Close()
			_ = gz.Close()
			return nil, fmt.Errorf("write virtual file %s data: %w", name, err)
		}
	}

	if err := tw.Close(); err != nil {
		_ = gz.Close()
		return nil, fmt.Errorf("close tar writer: %w", err)
	}
	if err := gz.Close(); err != nil {
		return nil, fmt.Errorf("close gzip writer: %w", err)
	}
	return buf.Bytes(), nil
}

func repoIncludeRoots(kind RepoKind) []string {
	switch kind {
	case RepoOpenSandbox:
		return []string{
			"LICENSE",
			"README.md",
			"server",
			"kubernetes",
			"components/internal",
			"components/ingress",
			"components/egress",
			"components/execd",
		}
	case RepoFastSandbox:
		return []string{
			"go.mod",
			"go.sum",
			"api",
			"cmd",
			"internal",
			"pkg",
			"build",
		}
	default:
		return []string{"."}
	}
}

func shouldSkipContextPath(slashRel string, isDir bool) bool {
	parts := strings.Split(slashRel, "/")
	for _, p := range parts {
		switch p {
		case ".git", ".fast-sandbox", "node_modules", ".venv", "__pycache__", ".pytest_cache", ".mypy_cache", ".ruff_cache", ".idea", ".vscode":
			return true
		}
	}
	if isDir {
		switch slashRel {
		case ".build", "bin", "kubernetes/bin", "server/.venv", "server/dist", "server/build":
			return true
		}
	}
	if strings.HasSuffix(slashRel, ".pyc") || strings.HasSuffix(slashRel, ".pyo") || strings.HasSuffix(slashRel, ".DS_Store") {
		return true
	}
	return false
}

// PrebuildFastSandboxBinary cross-compiles a static linux/amd64 Go binary for
// fast-sandbox components whose Dockerfiles copy .build/linux-amd64/<pkg>.
func PrebuildFastSandboxBinary(ctx context.Context, fsbDir, pkg string, goProxy string) (VirtualFile, error) {
	tmpDir, err := os.MkdirTemp("", "opensandbox-bench-fsb-bin-*")
	if err != nil {
		return VirtualFile{}, fmt.Errorf("create temp dir for %s: %w", pkg, err)
	}
	defer os.RemoveAll(tmpDir)

	outPath := filepath.Join(tmpDir, pkg)
	cmd := exec.CommandContext(ctx, "go", "build", "-trimpath", "-ldflags", "-s -w", "-o", outPath, "./cmd/"+pkg)
	cmd.Dir = fsbDir
	env := append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64")
	if strings.TrimSpace(goProxy) != "" {
		env = append(env, "GOPROXY="+strings.TrimSpace(goProxy))
	}
	cmd.Env = env

	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return VirtualFile{}, fmt.Errorf("cross-compile fast-sandbox ./cmd/%s (linux/amd64): %w\n%s", pkg, err, strings.TrimSpace(out.String()))
	}
	data, err := os.ReadFile(outPath)
	if err != nil {
		return VirtualFile{}, fmt.Errorf("read compiled binary %s: %w", pkg, err)
	}
	return VirtualFile{
		ArchivePath: fmt.Sprintf(".build/linux-amd64/%s", pkg),
		Data:        data,
		Mode:        0o755,
	}, nil
}

// GitShortCommit returns the short HEAD commit SHA for a repository directory, or "".
func GitShortCommit(ctx context.Context, dir string) string {
	if strings.TrimSpace(dir) == "" {
		return ""
	}
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--short=10", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
