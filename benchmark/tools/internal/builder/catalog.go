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
	"fmt"
	"path"
	"strings"
	"time"
)

// RepoKind identifies which source repository supplies a component's build context.
type RepoKind string

const (
	RepoOpenSandbox RepoKind = "opensandbox"
	RepoFastSandbox RepoKind = "fast-sandbox"
)

// ComponentSpec defines how a single OpenSandbox or fast-sandbox image is built.
type ComponentSpec struct {
	// Name is the canonical CLI component name and image basename (e.g. "server", "fsb-fastlet").
	Name string
	// Repo selects which source tree provides the build context.
	Repo RepoKind
	// ContextDir is relative to the repository root ("." for repo root).
	ContextDir string
	// Dockerfile is relative to the repository root.
	Dockerfile string
	// PrebuildBinary, when non-empty, names the Go package under ./cmd/<pkg> in the repo
	// that must be cross-compiled to linux/amd64 and injected at .build/linux-amd64/<pkg>.
	PrebuildBinary string
	// NeedsOCITools marks fast-sandbox images that consume --build-arg OCI_TOOLS_IMAGE=...
	NeedsOCITools bool
	// NeedsVersionArgs injects VERSION, GIT_COMMIT, BUILD_TIME build args.
	NeedsVersionArgs bool
	// NeedsPythonVersionArg forwards SETUPTOOLS_SCM_PRETEND_VERSION when tag is PEP 440 compatible.
	NeedsPythonVersionArg bool
	// SupportsGoProxy forwards GOPROXY when configured.
	SupportsGoProxy bool
	// SupportsPipIndex forwards PIP_INDEX_URL when configured.
	SupportsPipIndex bool
	// DefaultInFullStack indicates whether the component is included in "--components all".
	DefaultInFullStack bool
}

// DefaultBuildKitImage is the upstream BuildKit daemon+CLI image used inside the cluster.
const DefaultBuildKitImage = "moby/buildkit:v0.20.2"

// DefaultPlatform targets x86_64/amd64 ACK/Kubernetes bare-metal benchmark nodes.
const DefaultPlatform = "linux/amd64"

// DefaultRepoPrefix is the standard image namespace under the target registry (e.g. <acr>/opensandbox/<component>).
const DefaultRepoPrefix = "opensandbox"

// DefaultRegistrySecretName is the ACR dockerconfigjson secret created in OpenSandbox clusters.
const DefaultRegistrySecretName = "template-oci-publish-secret"

// Catalog returns the canonical full-stack component definitions in deterministic build order.
func Catalog() []ComponentSpec {
	return []ComponentSpec{
		// OpenSandbox components
		{
			Name:                  "server",
			Repo:                  RepoOpenSandbox,
			ContextDir:            "server",
			Dockerfile:            "server/Dockerfile",
			NeedsPythonVersionArg: true,
			DefaultInFullStack:    true,
		},
		{
			Name:               "controller",
			Repo:               RepoOpenSandbox,
			ContextDir:         "kubernetes",
			Dockerfile:         "kubernetes/Dockerfile",
			SupportsGoProxy:    true,
			DefaultInFullStack: true,
		},
		{
			Name:               "ingress",
			Repo:               RepoOpenSandbox,
			ContextDir:         ".",
			Dockerfile:         "components/ingress/Dockerfile",
			NeedsVersionArgs:   true,
			SupportsGoProxy:    true,
			DefaultInFullStack: true,
		},
		{
			Name:               "egress",
			Repo:               RepoOpenSandbox,
			ContextDir:         ".",
			Dockerfile:         "components/egress/Dockerfile",
			NeedsVersionArgs:   true,
			SupportsGoProxy:    true,
			SupportsPipIndex:   true,
			DefaultInFullStack: true,
		},
		{
			Name:               "execd",
			Repo:               RepoOpenSandbox,
			ContextDir:         ".",
			Dockerfile:         "components/execd/Dockerfile",
			NeedsVersionArgs:   true,
			SupportsGoProxy:    true,
			DefaultInFullStack: true,
		},
		// fast-sandbox shared OCI build base (automatically scheduled first when needed)
		{
			Name:               "oci-tools",
			Repo:               RepoFastSandbox,
			ContextDir:         ".",
			Dockerfile:         "build/Dockerfile.oci-tools",
			SupportsGoProxy:    true,
			DefaultInFullStack: false,
		},
		// fast-sandbox Firecracker-scope components (mirrors manifests/release/build-fast-sandbox.sh)
		{
			Name:               "fsb-controller",
			Repo:               RepoFastSandbox,
			ContextDir:         ".",
			Dockerfile:         "build/Dockerfile.controller",
			PrebuildBinary:     "controller",
			DefaultInFullStack: true,
		},
		{
			Name:               "fsb-fastlet",
			Repo:               RepoFastSandbox,
			ContextDir:         ".",
			Dockerfile:         "build/Dockerfile.fastlet",
			SupportsGoProxy:    true,
			DefaultInFullStack: true,
		},
		{
			Name:               "fsb-fastlet-proxy",
			Repo:               RepoFastSandbox,
			ContextDir:         ".",
			Dockerfile:         "build/Dockerfile.fastlet-proxy",
			PrebuildBinary:     "fastlet-proxy",
			DefaultInFullStack: true,
		},
		{
			Name:               "fsb-janitor",
			Repo:               RepoFastSandbox,
			ContextDir:         ".",
			Dockerfile:         "build/Dockerfile.janitor",
			PrebuildBinary:     "janitor",
			DefaultInFullStack: true,
		},
		{
			Name:               "fsb-firecracker-runtime",
			Repo:               RepoFastSandbox,
			ContextDir:         ".",
			Dockerfile:         "build/Dockerfile.firecracker-runtime",
			PrebuildBinary:     "firecracker-runtime",
			NeedsOCITools:      true,
			SupportsGoProxy:    true,
			DefaultInFullStack: true,
		},
		{
			Name:               "fsb-sandboxtemplate-builder",
			Repo:               RepoFastSandbox,
			ContextDir:         ".",
			Dockerfile:         "build/Dockerfile.sandboxtemplate-builder",
			NeedsOCITools:      true,
			SupportsGoProxy:    true,
			DefaultInFullStack: true,
		},
	}
}

// ResolveComponents resolves user-selected component names (including group aliases
// "all", "opensandbox", "fast-sandbox") into an ordered build plan. When any selected
// component has NeedsOCITools=true and ociToolsOverride is empty, "oci-tools" is
// automatically prepended as a prerequisite build unless already present.
func ResolveComponents(selection []string, ociToolsOverride string) ([]ComponentSpec, error) {
	cat := Catalog()
	byName := make(map[string]ComponentSpec, len(cat))
	for _, c := range cat {
		byName[c.Name] = c
	}

	if len(selection) == 0 {
		selection = []string{"all"}
	}

	seen := make(map[string]bool)
	var picked []ComponentSpec
	add := func(spec ComponentSpec) {
		if !seen[spec.Name] {
			seen[spec.Name] = true
			picked = append(picked, spec)
		}
	}

	for _, raw := range selection {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		switch strings.ToLower(name) {
		case "all":
			for _, c := range cat {
				if c.DefaultInFullStack {
					add(c)
				}
			}
		case "opensandbox", "osb":
			for _, c := range cat {
				if c.Repo == RepoOpenSandbox && c.DefaultInFullStack {
					add(c)
				}
			}
		case "fast-sandbox", "fsb":
			for _, c := range cat {
				if c.Repo == RepoFastSandbox && c.DefaultInFullStack {
					add(c)
				}
			}
		default:
			spec, ok := byName[name]
			if !ok {
				return nil, fmt.Errorf("unknown component %q (valid: all, opensandbox, fast-sandbox, or %s)", name, strings.Join(AllComponentNames(), ", "))
			}
			add(spec)
		}
	}

	if len(picked) == 0 {
		return nil, fmt.Errorf("no components selected to build")
	}

	needsOCITools := false
	for _, c := range picked {
		if c.NeedsOCITools {
			needsOCITools = true
			break
		}
	}
	if needsOCITools && strings.TrimSpace(ociToolsOverride) == "" && !seen["oci-tools"] {
		picked = append([]ComponentSpec{byName["oci-tools"]}, picked...)
	} else if seen["oci-tools"] {
		// Ensure oci-tools always precedes dependent builds.
		ordered := make([]ComponentSpec, 0, len(picked))
		ordered = append(ordered, byName["oci-tools"])
		for _, c := range picked {
			if c.Name != "oci-tools" {
				ordered = append(ordered, c)
			}
		}
		picked = ordered
	}

	return picked, nil
}

// AllComponentNames returns all individual component names in catalog order.
func AllComponentNames() []string {
	cat := Catalog()
	out := make([]string, 0, len(cat))
	for _, c := range cat {
		out = append(out, c.Name)
	}
	return out
}

// FullImageRef builds "<registry>/<repoPrefix>/<component>:<tag>".
func FullImageRef(registry, repoPrefix, component, tag string) string {
	reg := strings.TrimRight(strings.TrimSpace(registry), "/")
	prefix := strings.Trim(strings.TrimSpace(repoPrefix), "/")
	repo := component
	if prefix != "" {
		repo = path.Join(prefix, component)
	}
	if reg != "" {
		repo = reg + "/" + repo
	}
	if strings.TrimSpace(tag) != "" {
		return repo + ":" + strings.TrimSpace(tag)
	}
	return repo
}

// ComponentBuildResult captures the outcome of building and pushing one image.
type ComponentBuildResult struct {
	Component       string        `json:"component"`
	Repo            RepoKind      `json:"repo"`
	Image           string        `json:"image"`
	Digest          string        `json:"digest,omitempty"`
	Platform        string        `json:"platform"`
	DurationSeconds float64       `json:"duration_seconds"`
	Duration        time.Duration `json:"-"`
}

// BuildManifest is the machine-readable record emitted by `opensandbox-bench build`.
type BuildManifest struct {
	TimestampUTC      string                 `json:"timestamp_utc"`
	Platform          string                 `json:"platform"`
	Registry          string                 `json:"registry"`
	RepoPrefix        string                 `json:"repo_prefix"`
	Tag               string                 `json:"tag"`
	OpenSandboxCommit string                 `json:"opensandbox_commit,omitempty"`
	FastSandboxCommit string                 `json:"fast_sandbox_commit,omitempty"`
	Images            []ComponentBuildResult `json:"images"`
}

// BuildHelmOverrides constructs a Helm values map compatible with
// manifests/charts/opensandbox (matching provisioner.BuildHelmValues keys)
// so built images can be deployed immediately via `helm upgrade -f ...` or BenchmarkProfile.
func BuildHelmOverrides(registry, repoPrefix, tag string, results []ComponentBuildResult) map[string]any {
	built := make(map[string]ComponentBuildResult, len(results))
	for _, r := range results {
		built[r.Component] = r
	}
	repoOf := func(comp string) string {
		return FullImageRef(registry, repoPrefix, comp, "")
	}
	refOf := func(comp string) string {
		if r, ok := built[comp]; ok && r.Image != "" {
			return r.Image
		}
		return FullImageRef(registry, repoPrefix, comp, tag)
	}

	values := make(map[string]any)
	if _, ok := built["server"]; ok {
		values["opensandbox-server"] = map[string]any{
			"server": map[string]any{
				"image": map[string]any{
					"repository": repoOf("server"),
					"tag":        tag,
				},
			},
		}
	}
	if _, ok := built["controller"]; ok {
		values["opensandbox-controller"] = map[string]any{
			"controller": map[string]any{
				"image": map[string]any{
					"repository": repoOf("controller"),
					"tag":        tag,
				},
			},
		}
	}
	if _, ok := built["ingress"]; ok {
		values["ingress-gateway"] = map[string]any{
			"gateway": map[string]any{
				"image": map[string]any{
					"repository": repoOf("ingress"),
					"tag":        tag,
				},
			},
		}
	}

	fsb := make(map[string]any)
	fsbCtrl := make(map[string]any)
	if _, ok := built["fsb-controller"]; ok {
		fsbCtrl["image"] = map[string]any{
			"repository": repoOf("fsb-controller"),
			"tag":        tag,
		}
	}
	if _, ok := built["fsb-fastlet-proxy"]; ok {
		fsbCtrl["fastletProxyImage"] = refOf("fsb-fastlet-proxy")
	}
	if _, ok := built["fsb-sandboxtemplate-builder"]; ok {
		fsbCtrl["sandboxtemplateBuilderImage"] = refOf("fsb-sandboxtemplate-builder")
	}
	if len(fsbCtrl) > 0 {
		fsb["controller"] = fsbCtrl
	}
	if _, ok := built["fsb-janitor"]; ok {
		fsb["janitor"] = map[string]any{
			"image": map[string]any{
				"repository": repoOf("fsb-janitor"),
				"tag":        tag,
			},
		}
	}
	if _, ok := built["fsb-firecracker-runtime"]; ok {
		fsb["runtime"] = map[string]any{
			"image": map[string]any{
				"repository": repoOf("fsb-firecracker-runtime"),
				"tag":        tag,
			},
		}
	}
	if len(fsb) > 0 {
		values["fast-sandbox"] = fsb
	}

	// BenchmarkProfile-level runtime images (execd, egress, fsb-fastlet) are configured
	// via server configToml / SandboxTemplate / SandboxPool; expose them under a top-level
	// helper block for easy copy-paste or automated consumption.
	profileRefs := make(map[string]any)
	for _, comp := range []string{"execd", "egress", "fsb-fastlet"} {
		if _, ok := built[comp]; ok {
			profileRefs[comp] = refOf(comp)
		}
	}
	if len(profileRefs) > 0 {
		values["benchmarkProfileImages"] = profileRefs
	}
	return values
}
