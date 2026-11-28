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
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/builder"
	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/profile"
	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/runner"
)

func (a *App) newBuildCmd() *cobra.Command {
	var (
		kubeconfig       string
		profilePath      string
		namespace        string
		registry         string
		repoPrefix       string
		tag              string
		platform         string
		componentsFlag   string
		osbDir           string
		fsbDir           string
		registrySecret   string
		registrySecretNS string
		buildkitImage    string
		cacheHostPath    string
		ociToolsImage    string
		goProxy          string
		pipIndexURL      string
		concurrency      int
		keepBuilder      bool
		noPush           bool
		plainHTTP        bool
		listOnly         bool
		verbose          bool
		writeManifest    string
		writeValues      string
	)

	cmd := &cobra.Command{
		Use:   "build",
		Short: "Build full-stack x86_64/amd64 OpenSandbox + fast-sandbox images in-cluster with BuildKit and push to ACR",
		Long: `Launch a temporary privileged BuildKit pod on an amd64 cluster node, stream
filtered local OpenSandbox and fast-sandbox source contexts (honoring each
component's .dockerignore rules), build the selected linux/amd64 container images,
and push them to ACR or any OCI registry authenticated by an existing Kubernetes
dockerconfigjson secret.

Prerequisites:
  - kubeconfig permissions in --namespace to get/create/delete pods, secrets,
    and pods/exec, plus get secrets in --registry-secret-namespace.
  - At least one linux/amd64 node allowing a privileged moby/buildkit pod.
  - An existing kubernetes.io/dockerconfigjson secret (default:
    template-oci-publish-secret) or explicit --registry/--plain-http flags.
  - Local OpenSandbox (--opensandbox-dir) and/or fast-sandbox (--fast-sandbox-dir)
    source checkouts matching the selected --components.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			selected := splitCSV(componentsFlag)
			plan, err := builder.ResolveComponents(selected, ociToolsImage)
			if err != nil {
				return err
			}

			if listOnly {
				tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 2, 2, ' ', 0)
				fmt.Fprintln(tw, "COMPONENT\tREPO\tDOCKERFILE\tCONTEXT\tIMAGE")
				previewReg := registry
				if previewReg == "" {
					previewReg = "<auto-from-secret>"
				}
				previewTag := tag
				if previewTag == "" {
					previewTag = "<bench-commit>"
				}
				for _, c := range plan {
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
						c.Name, c.Repo, c.Dockerfile, c.ContextDir,
						builder.FullImageRef(previewReg, repoPrefix, c.Name, previewTag))
				}
				return tw.Flush()
			}

			if namespace == "" && profilePath != "" {
				prof, err := profile.Load(profilePath)
				if err != nil {
					return err
				}
				namespace = prof.Environment.BenchNamespace
			}
			if namespace == "" {
				namespace = "opensandbox-bench-run"
			}

			needsOSB, needsFSB := false, false
			for _, c := range plan {
				if c.Repo == builder.RepoOpenSandbox {
					needsOSB = true
				}
				if c.Repo == builder.RepoFastSandbox {
					needsFSB = true
				}
			}

			if needsOSB && strings.TrimSpace(osbDir) == "" {
				osbDir = detectOpenSandboxRepoDir()
			}
			if needsFSB && strings.TrimSpace(fsbDir) == "" {
				fsbDir = detectFastSandboxRepoDir(osbDir)
			}

			kc, err := runner.LoadKubeClients(kubeconfig)
			if err != nil {
				return err
			}

			bk := builder.NewBuildKitRunner(kc, a.Logger)
			opts := builder.Options{
				Namespace:               namespace,
				Registry:                registry,
				RepoPrefix:              repoPrefix,
				Tag:                     tag,
				Platform:                platform,
				Components:              selected,
				OpenSandboxDir:          osbDir,
				FastSandboxDir:          fsbDir,
				RegistrySecretName:      registrySecret,
				RegistrySecretNamespace: registrySecretNS,
				BuildKitImage:           buildkitImage,
				CacheHostPath:           cacheHostPath,
				OCIToolsImageOverride:   ociToolsImage,
				GoProxy:                 goProxy,
				PipIndexURL:             pipIndexURL,
				Concurrency:             concurrency,
				KeepBuilder:             keepBuilder,
				NoPush:                  noPush,
				PlainHTTP:               plainHTTP,
			}
			if verbose {
				opts.ProgressWriter = cmd.ErrOrStderr()
			}

			manifest, err := bk.Run(cmd.Context(), opts)
			if err != nil {
				return err
			}

			if writeManifest != "" {
				if err := os.MkdirAll(filepath.Dir(writeManifest), 0o755); err != nil {
					return fmt.Errorf("create manifest parent dir: %w", err)
				}
				raw, err := json.MarshalIndent(manifest, "", "  ")
				if err != nil {
					return fmt.Errorf("marshal build manifest: %w", err)
				}
				if err := os.WriteFile(writeManifest, append(raw, '\n'), 0o644); err != nil {
					return fmt.Errorf("write build manifest %s: %w", writeManifest, err)
				}
			}

			if writeValues != "" {
				if err := os.MkdirAll(filepath.Dir(writeValues), 0o755); err != nil {
					return fmt.Errorf("create values parent dir: %w", err)
				}
				overrides := builder.BuildHelmOverrides(manifest.Registry, manifest.RepoPrefix, manifest.Tag, manifest.Images)
				raw, err := yaml.Marshal(overrides)
				if err != nil {
					return fmt.Errorf("marshal helm override values: %w", err)
				}
				if err := os.WriteFile(writeValues, raw, 0o644); err != nil {
					return fmt.Errorf("write helm override values %s: %w", writeValues, err)
				}
			}

			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "COMPONENT\tIMAGE\tDIGEST\tDURATION")
			for _, img := range manifest.Images {
				digest := img.Digest
				if digest == "" {
					digest = "-"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%.1fs\n", img.Component, img.Image, digest, img.DurationSeconds)
			}
			return tw.Flush()
		},
	}

	cmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "Path to kubeconfig file")
	cmd.Flags().StringVar(&profilePath, "profile", "", "Optional BenchmarkProfile YAML (used to default build namespace)")
	cmd.Flags().StringVar(&namespace, "namespace", "", "Kubernetes namespace for temporary BuildKit pod (default opensandbox-bench-run)")
	cmd.Flags().StringVar(&registry, "registry", "", "Target OCI registry host (auto-detected from --registry-secret when omitted)")
	cmd.Flags().StringVar(&repoPrefix, "repo-prefix", builder.DefaultRepoPrefix, "Repository namespace prefix under registry")
	cmd.Flags().StringVar(&tag, "tag", "", "Image tag to build and push (default bench-<short-sha>)")
	cmd.Flags().StringVar(&platform, "platform", builder.DefaultPlatform, "Target image platform")
	cmd.Flags().StringVar(&componentsFlag, "components", "all", "Comma-separated components or groups: all, opensandbox, fast-sandbox, or individual component names")
	cmd.Flags().StringVar(&osbDir, "opensandbox-dir", "", "Path to OpenSandbox source repository (auto-detected from cwd when omitted)")
	cmd.Flags().StringVar(&fsbDir, "fast-sandbox-dir", "", "Path to fast-sandbox source repository (auto-detected from ../fast-sandbox or .fast-sandbox/src when omitted)")
	cmd.Flags().StringVar(&registrySecret, "registry-secret", builder.DefaultRegistrySecretName, "Existing Kubernetes dockerconfigjson secret used for ACR authentication")
	cmd.Flags().StringVar(&registrySecretNS, "registry-secret-namespace", "", "Namespace of --registry-secret (auto-searches build namespace, opensandbox-system, opensandbox-dataplane, default)")
	cmd.Flags().StringVar(&buildkitImage, "buildkit-image", builder.DefaultBuildKitImage, "BuildKit container image run inside the cluster")
	cmd.Flags().StringVar(&cacheHostPath, "cache-host-path", "", "Optional node hostPath mounted at /var/lib/buildkit for cross-run BuildKit layer cache")
	cmd.Flags().StringVar(&ociToolsImage, "oci-tools-image", "", "Optional pre-built oci-tools image ref (skips building oci-tools)")
	cmd.Flags().StringVar(&goProxy, "go-proxy", "", "Optional GOPROXY build arg forwarded to Go builds")
	cmd.Flags().StringVar(&pipIndexURL, "pip-index-url", "", "Optional PIP_INDEX_URL build arg forwarded to egress build")
	cmd.Flags().IntVar(&concurrency, "concurrency", 2, "Maximum concurrent BuildKit builds after oci-tools")
	cmd.Flags().BoolVar(&keepBuilder, "keep-builder", false, "Keep the temporary BuildKit pod after build completes")
	cmd.Flags().BoolVar(&noPush, "no-push", false, "Build images inside BuildKit without pushing to the registry")
	cmd.Flags().BoolVar(&plainHTTP, "plain-http", false, "Configure BuildKit for plain-HTTP / insecure registries (auto-enabled for in-cluster *.svc:5000 registries)")
	cmd.Flags().BoolVar(&listOnly, "list", false, "Print resolved build matrix and exit without starting BuildKit")
	cmd.Flags().BoolVar(&verbose, "verbose", false, "Stream live buildctl progress logs to stderr")
	cmd.Flags().StringVar(&writeManifest, "write-manifest", "", "Optional file path to write JSON build manifest with image digests")
	cmd.Flags().StringVar(&writeValues, "write-values", "", "Optional file path to write Helm values override YAML for built images")

	return cmd
}

func detectOpenSandboxRepoDir() string {
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	dir := cwd
	for {
		if fileExists(filepath.Join(dir, "server", "Dockerfile")) && fileExists(filepath.Join(dir, "components", "ingress", "Dockerfile")) {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

func detectFastSandboxRepoDir(osbDir string) string {
	var candidates []string
	if osbDir != "" {
		candidates = append(candidates,
			filepath.Join(osbDir, ".fast-sandbox", "src"),
			filepath.Join(filepath.Dir(osbDir), "fast-sandbox"),
		)
	}
	if cwd, err := os.Getwd(); err == nil {
		candidates = append(candidates,
			filepath.Join(cwd, "fast-sandbox"),
			filepath.Join(filepath.Dir(cwd), "fast-sandbox"),
		)
	}
	for _, c := range candidates {
		if fileExists(filepath.Join(c, "build", "Dockerfile.fastlet")) {
			return c
		}
	}
	return ""
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}
