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

package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/report"
)

// LocalProvenance captures git/pinned-commit metadata detected on the operator's workstation.
type LocalProvenance struct {
	OpenSandboxCommit string
	OpenSandboxTag    string
	FastSandboxCommit string
	FastSandboxTag    string
	BenchGitCommit    string
}

// DetectLocalProvenance best-effort detects OpenSandbox git commit/tag and pinned fast-sandbox
// commit from manifests/third-party/fast-sandbox.commit relative to repoRoot (or cwd if empty).
func DetectLocalProvenance(repoRoot string) LocalProvenance {
	var p LocalProvenance
	dir := strings.TrimSpace(repoRoot)
	if dir == "" {
		dir = findGitTopLevel()
	}

	if sha := gitOutput(dir, "rev-parse", "HEAD"); sha != "" {
		p.OpenSandboxCommit = sha
		p.BenchGitCommit = sha
	}
	if tag := gitOutput(dir, "describe", "--tags", "--exact-match"); tag != "" {
		p.OpenSandboxTag = tag
	} else if tag := gitOutput(dir, "describe", "--tags", "--abbrev=0"); tag != "" {
		p.OpenSandboxTag = tag
	}

	if commitPath := findFastSandboxCommitFile(dir); commitPath != "" {
		p.FastSandboxCommit = parseFastSandboxCommitFile(commitPath)
	}
	return p
}

func findGitTopLevel() string {
	return gitOutput("", "rev-parse", "--show-toplevel")
}

func gitOutput(dir string, args ...string) string {
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func findFastSandboxCommitFile(repoRoot string) string {
	rel := filepath.Join("manifests", "third-party", "fast-sandbox.commit")
	candidates := []string{}
	if repoRoot != "" {
		candidates = append(candidates, filepath.Join(repoRoot, rel))
	}
	candidates = append(candidates,
		rel,
		filepath.Join("..", rel),
		filepath.Join("..", "..", rel),
	)
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return ""
}

func parseFastSandboxCommitFile(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		if strings.TrimSpace(k) == "commit" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func renderPreflightSummary(w io.Writer, env *report.EnvironmentFingerprint, asJSON bool) error {
	if env == nil {
		env = &report.EnvironmentFingerprint{}
	}
	if asJSON {
		raw, err := json.MarshalIndent(env, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal environment fingerprint: %w", err)
		}
		_, err = fmt.Fprintf(w, "%s\n", raw)
		return err
	}

	fmt.Fprintf(w, "Preflight PASSED\n")
	fmt.Fprintf(w, "  Kubernetes: %s  Arch: %s  PoolMode: %s\n",
		emptyDash(env.KubernetesVersion), emptyDash(env.ClusterArch), emptyDash(env.PoolMode))
	fmt.Fprintf(w, "  Nodes inspected: %d\n", len(env.Nodes))

	cpuIdentities := make(map[string]struct{})
	for _, n := range env.Nodes {
		if n.CPUIdentity != "" {
			cpuIdentities[n.CPUIdentity] = struct{}{}
		}
		fmt.Fprintf(w, "    - %s (cpu=%s mem=%s kernel=%s): kvm=%t ublk=%t fs=%s reflink=%t spill=%t cpu_id=%s\n",
			n.Name,
			emptyDash(n.CPUCapacity),
			emptyDash(n.MemoryCapacity),
			emptyDash(n.KernelVersion),
			n.KVMReady,
			n.UblkReady,
			emptyDash(n.StateRootFS),
			n.ReflinkSupported,
			n.SpillDirAvailable,
			emptyDash(n.CPUIdentity),
		)
	}
	switch len(cpuIdentities) {
	case 0:
		fmt.Fprintf(w, "  CPU homogeneity: unknown\n")
	case 1:
		fmt.Fprintf(w, "  CPU homogeneity: homogeneous\n")
	default:
		fmt.Fprintf(w, "  CPU homogeneity: HETEROGENEOUS (%d distinct CPU identities; cross-node checkpoint resume may fail)\n", len(cpuIdentities))
	}
	return nil
}

func renderLeakAuditSummary(w io.Writer, audit *report.LeakAudit) {
	if audit == nil {
		fmt.Fprintln(w, "Leak audit: skipped (no audit report returned)")
		return
	}
	status := "PASSED"
	if !audit.Passed {
		status = "FAILED"
	}
	fmt.Fprintf(w, "Leak audit: %s\n", status)
	fmt.Fprintf(w, "  Host OOM kills: %d | Pod cgroup OOM kills: %d\n", audit.HostOOMKills, audit.PodCgroupOOMKills)
	fmt.Fprintf(w, "  Residual CRs (Sandbox / Snapshot): %d / %d\n", audit.ResidualSandboxCRs, audit.ResidualSnapshotCRs)
	if audit.CRConvergenceSeconds > 0 {
		fmt.Fprintf(w, "  CR cleanup convergence: %.2fs\n", audit.CRConvergenceSeconds)
	}
	fmt.Fprintf(w, "  Stuck deleting slots: %d | Orphaned ublk devices: %d | Leaked Firecracker PIDs: %d\n",
		audit.StuckDeletingSlots, audit.OrphanedUblkDevices, audit.LeakedFirecrackerPIDs)
	if audit.StateRootFreeHeadroomG > 0 {
		fmt.Fprintf(w, "  StateRoot free headroom: %.2f GiB\n", audit.StateRootFreeHeadroomG)
	}
	for _, f := range audit.Failures {
		fmt.Fprintf(w, "  Failure: %s\n", f)
	}
}

func emptyDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}
