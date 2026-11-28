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
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
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
// tar archive for upload into the in-cluster BuildKit pod. Before archiving, it honors
// each selected component's Dockerfile-specific `.dockerignore` (or context/repo
// `.dockerignore`) so ignored local files (such as `.env` files, credentials, or test
// fixtures) never leave the workstation.
func ArchiveRepoContext(repoDir string, kind RepoKind, components []ComponentSpec, extraFiles []VirtualFile) ([]byte, error) {
	absRoot, err := filepath.Abs(repoDir)
	if err != nil {
		return nil, fmt.Errorf("resolve repo dir %q: %w", repoDir, err)
	}
	st, err := os.Stat(absRoot)
	if err != nil || !st.IsDir() {
		return nil, fmt.Errorf("repo dir %q does not exist or is not a directory", repoDir)
	}

	var repoComps []ComponentSpec
	for _, c := range components {
		if c.Repo == kind {
			repoComps = append(repoComps, c)
		}
	}
	matchers, err := buildComponentIgnoreMatchers(absRoot, repoComps)
	if err != nil {
		return nil, err
	}

	roots := repoIncludeRoots(kind, repoComps)

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	written := make(map[string]bool)
	extraSet := make(map[string]bool, len(extraFiles))
	for _, ef := range extraFiles {
		extraSet[filepath.ToSlash(strings.TrimLeft(ef.ArchivePath, "/"))] = true
	}

	ensureParentDirs := func(slashRel string) error {
		dir := path.Dir(slashRel)
		if dir == "." || dir == "/" {
			return nil
		}
		parts := strings.Split(dir, "/")
		cur := ""
		for _, p := range parts {
			if cur == "" {
				cur = p
			} else {
				cur += "/" + p
			}
			if written[cur] {
				continue
			}
			written[cur] = true
			if err := tw.WriteHeader(&tar.Header{
				Name:     cur + "/",
				Mode:     0o755,
				Typeflag: tar.TypeDir,
			}); err != nil {
				return err
			}
		}
		return nil
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
			if d.IsDir() {
				// Do not skip directories prematurely when allowlist-style ignore files
				// (starting with "**") are active, because negation rules ("!dir/**")
				// re-include descendants inside the directory.
				return nil
			}
			if extraSet[slashRel] || written[slashRel] {
				return nil
			}
			if len(matchers) > 0 && !anyMatcherAllows(matchers, slashRel) {
				return nil
			}

			info, err := d.Info()
			if err != nil {
				return err
			}
			if err := ensureParentDirs(slashRel); err != nil {
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

			if !info.Mode().IsRegular() {
				return nil
			}

			hdr, err := tar.FileInfoHeader(info, "")
			if err != nil {
				return err
			}
			hdr.Name = slashRel
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
		if err := ensureParentDirs(name); err != nil {
			_ = tw.Close()
			_ = gz.Close()
			return nil, fmt.Errorf("write virtual dir for %s: %w", name, err)
		}
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

type dockerIgnoreRule struct {
	pattern string
	negate  bool
}

type componentIgnoreMatcher struct {
	contextDir       string
	dockerfilePath   string
	ignoreFilePath   string
	rules            []dockerIgnoreRule
	hasExplicitRules bool
}

func buildComponentIgnoreMatchers(repoRoot string, comps []ComponentSpec) ([]componentIgnoreMatcher, error) {
	if len(comps) == 0 {
		// Fall back to repository root .dockerignore when no specific components are supplied.
		rules, ok, err := readDockerIgnoreFile(filepath.Join(repoRoot, ".dockerignore"))
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, nil
		}
		return []componentIgnoreMatcher{{
			contextDir:       ".",
			rules:            rules,
			hasExplicitRules: true,
		}}, nil
	}

	matchers := make([]componentIgnoreMatcher, 0, len(comps))
	for _, c := range comps {
		ctxDir := strings.Trim(path.Clean(strings.TrimSpace(c.ContextDir)), "/")
		if ctxDir == "" {
			ctxDir = "."
		}
		dfPath := strings.TrimLeft(path.Clean(strings.TrimSpace(c.Dockerfile)), "/")

		// Follow Docker/BuildKit precedence: <Dockerfile>.dockerignore first,
		// then <ContextDir>/.dockerignore, then <repoRoot>/.dockerignore.
		candidates := []string{
			dfPath + ".dockerignore",
		}
		if ctxDir != "." {
			candidates = append(candidates, path.Join(ctxDir, ".dockerignore"))
		}
		candidates = append(candidates, ".dockerignore")

		var (
			rules    []dockerIgnoreRule
			found    bool
			usedPath string
		)
		for _, rel := range candidates {
			r, ok, err := readDockerIgnoreFile(filepath.Join(repoRoot, filepath.FromSlash(rel)))
			if err != nil {
				return nil, fmt.Errorf("parse %s: %w", rel, err)
			}
			if ok {
				rules = r
				found = true
				usedPath = rel
				break
			}
		}

		matchers = append(matchers, componentIgnoreMatcher{
			contextDir:       ctxDir,
			dockerfilePath:   dfPath,
			ignoreFilePath:   usedPath,
			rules:            rules,
			hasExplicitRules: found,
		})
	}
	return matchers, nil
}

func readDockerIgnoreFile(filePath string) ([]dockerIgnoreRule, bool, error) {
	f, err := os.Open(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	defer f.Close()

	var rules []dockerIgnoreRule
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		negate := false
		if strings.HasPrefix(line, "!") {
			negate = true
			line = strings.TrimSpace(line[1:])
		}
		line = strings.TrimPrefix(line, "./")
		line = strings.TrimLeft(line, "/")
		line = strings.TrimRight(line, "/")
		if line == "" {
			continue
		}
		rules = append(rules, dockerIgnoreRule{pattern: line, negate: negate})
	}
	if err := sc.Err(); err != nil {
		return nil, false, err
	}
	return rules, true, nil
}

func anyMatcherAllows(matchers []componentIgnoreMatcher, repoRelPath string) bool {
	for _, m := range matchers {
		if m.allows(repoRelPath) {
			return true
		}
	}
	return false
}

func (m componentIgnoreMatcher) allows(repoRelPath string) bool {
	p := strings.TrimLeft(path.Clean(repoRelPath), "/")
	if p == m.dockerfilePath || (m.ignoreFilePath != "" && p == m.ignoreFilePath) {
		return true
	}

	// Restrict files outside a component's own contextDir.
	relToCtx := p
	if m.contextDir != "." {
		prefix := m.contextDir + "/"
		if !strings.HasPrefix(p, prefix) {
			return false
		}
		relToCtx = strings.TrimPrefix(p, prefix)
	}

	if !m.hasExplicitRules {
		return true
	}

	ignored := false
	for _, r := range m.rules {
		if dockerIgnoreMatch(r.pattern, relToCtx) || (m.contextDir != "." && dockerIgnoreMatch(r.pattern, p)) {
			ignored = !r.negate
		}
	}
	return !ignored
}

// dockerIgnoreMatch evaluates a single .dockerignore pattern (supporting "*", "?",
// "**", and directory-prefix matching) against a slash-separated relative path.
func dockerIgnoreMatch(pattern, target string) bool {
	if pattern == "**" {
		return true
	}
	patSegs := strings.Split(pattern, "/")
	tgtSegs := strings.Split(target, "/")

	// Match full path OR any parent directory prefix (e.g. ".git" matches ".git/config").
	if matchSegments(patSegs, tgtSegs) {
		return true
	}
	for i := 1; i < len(tgtSegs); i++ {
		if matchSegments(patSegs, tgtSegs[:i]) {
			return true
		}
	}
	return false
}

func matchSegments(pat, tgt []string) bool {
	if len(pat) == 0 {
		return len(tgt) == 0
	}
	if pat[0] == "**" {
		// "**" matches zero or more path segments.
		if matchSegments(pat[1:], tgt) {
			return true
		}
		if len(tgt) > 0 {
			return matchSegments(pat, tgt[1:])
		}
		return false
	}
	if len(tgt) == 0 {
		return false
	}
	matched, err := path.Match(pat[0], tgt[0])
	if err != nil || !matched {
		return false
	}
	return matchSegments(pat[1:], tgt[1:])
}

func repoIncludeRoots(kind RepoKind, comps []ComponentSpec) []string {
	switch kind {
	case RepoOpenSandbox:
		if len(comps) > 0 {
			seen := make(map[string]bool)
			var roots []string
			add := func(r string) {
				if !seen[r] {
					seen[r] = true
					roots = append(roots, r)
				}
			}
			for _, c := range comps {
				switch c.Name {
				case "server":
					add("server")
				case "controller":
					add("kubernetes")
				case "ingress":
					add("kubernetes")
					add("components/internal")
					add("components/ingress")
				case "egress":
					add("components/internal")
					add("components/egress")
				case "execd":
					add("components/internal")
					add("components/execd")
				default:
					add(c.ContextDir)
				}
			}
			return roots
		}
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

// GitIsDirty reports whether the git working tree at dir has uncommitted changes.
func GitIsDirty(ctx context.Context, dir string) bool {
	if strings.TrimSpace(dir) == "" {
		return false
	}
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "status", "--porcelain")
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	return len(strings.TrimSpace(string(out))) > 0
}
