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
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/runner"
)

func TestResolveComponents_DependencyOrderAndAliases(t *testing.T) {
	all, err := ResolveComponents([]string{"all"}, "")
	require.NoError(t, err)
	require.Equal(t, "oci-tools", all[0].Name, "oci-tools must be prepended automatically when firecracker-runtime/sandboxtemplate-builder are selected")
	require.Len(t, all, 12) // 5 OSB + 6 FSB + oci-tools

	// With ociToolsOverride, oci-tools should NOT be automatically injected.
	withOverride, err := ResolveComponents([]string{"fsb-firecracker-runtime"}, "acr.example/opensandbox/oci-tools:pinned")
	require.NoError(t, err)
	require.Len(t, withOverride, 1)
	require.Equal(t, "fsb-firecracker-runtime", withOverride[0].Name)

	// OpenSandbox-only alias should never pull oci-tools.
	osbOnly, err := ResolveComponents([]string{"opensandbox"}, "")
	require.NoError(t, err)
	require.Len(t, osbOnly, 5)
	for _, c := range osbOnly {
		require.Equal(t, RepoOpenSandbox, c.Repo)
	}

	// Unknown component fails clearly.
	_, err = ResolveComponents([]string{"does-not-exist"}, "")
	require.Error(t, err)
}

func TestArchiveRepoContext_SelectiveInclusionAndVirtualBinary(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "build"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".git"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "cmd/controller"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/fsb\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "build/Dockerfile.controller"), []byte("FROM alpine:3.19\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".git/config"), []byte("secret"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".dockerignore"), []byte(".git\n*.env\n**/*.env\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "local.env"), []byte("SECRET=0\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "cmd/controller/local.env"), []byte("SECRET=1\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "cmd/controller/main.go"), []byte("package main\n"), 0o644))

	comps, err := ResolveComponents([]string{"fsb-controller"}, "")
	require.NoError(t, err)
	tarGz, err := ArchiveRepoContext(dir, RepoFastSandbox, comps, []VirtualFile{
		{ArchivePath: ".build/linux-amd64/controller", Data: []byte("ELF-BIN"), Mode: 0o755},
	})
	require.NoError(t, err)

	gzr, err := gzip.NewReader(bytes.NewReader(tarGz))
	require.NoError(t, err)
	defer gzr.Close()
	tr := tar.NewReader(gzr)

	entries := make(map[string][]byte)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		if hdr.Typeflag == tar.TypeReg {
			data, err := io.ReadAll(tr)
			require.NoError(t, err)
			entries[hdr.Name] = data
		}
	}

	require.Contains(t, entries, "go.mod")
	require.Contains(t, entries, "build/Dockerfile.controller")
	require.Contains(t, entries, "cmd/controller/main.go")
	require.Equal(t, []byte("ELF-BIN"), entries[".build/linux-amd64/controller"])
	require.NotContains(t, entries, ".git/config")
	require.NotContains(t, entries, "cmd/controller/local.env")
}

func TestArchiveRepoContext_HonorsDockerfileAllowlistIgnore(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "server/opensandbox_server"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "server/Dockerfile"), []byte("FROM python:3.10-slim\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "server/Dockerfile.dockerignore"), []byte("**\n!Dockerfile\n!pyproject.toml\n!opensandbox_server/**\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "server/pyproject.toml"), []byte("[project]\nname='x'\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "server/opensandbox_server/app.py"), []byte("print('ok')\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "server/.env"), []byte("API_KEY=topsecret\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "server/local-creds.json"), []byte("{}\n"), 0o644))

	comps, err := ResolveComponents([]string{"server"}, "")
	require.NoError(t, err)
	tarGz, err := ArchiveRepoContext(dir, RepoOpenSandbox, comps, nil)
	require.NoError(t, err)

	gzr, err := gzip.NewReader(bytes.NewReader(tarGz))
	require.NoError(t, err)
	defer gzr.Close()
	tr := tar.NewReader(gzr)
	entries := make(map[string]bool)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		if hdr.Typeflag == tar.TypeReg {
			entries[hdr.Name] = true
		}
	}

	require.True(t, entries["server/Dockerfile"])
	require.True(t, entries["server/pyproject.toml"])
	require.True(t, entries["server/opensandbox_server/app.py"])
	require.False(t, entries["server/.env"], "ignored .env must not be uploaded")
	require.False(t, entries["server/local-creds.json"], "ignored credential fixture must not be uploaded")
}

type fakeBuildExecer struct {
	mu       sync.Mutex
	commands [][]string
	uploads  int
	metaJSON map[string]string
}

func (f *fakeBuildExecer) Exec(_ context.Context, req runner.ExecRequest) error {
	f.mu.Lock()
	f.commands = append(f.commands, append([]string(nil), req.Command...))
	f.mu.Unlock()

	if len(req.Command) >= 3 && req.Command[0] == "sh" && strings.Contains(req.Command[2], "tar -xzf") {
		f.mu.Lock()
		f.uploads++
		f.mu.Unlock()
		if req.Stdin != nil {
			_, _ = io.Copy(io.Discard, req.Stdin)
		}
		return nil
	}
	if len(req.Command) >= 4 && req.Command[0] == "buildctl" && req.Command[3] == "build" {
		// Write concurrently to Stdout and Stderr to mirror client-go SPDY streaming
		// and verify zero data races under `go test -race`.
		var streamWG sync.WaitGroup
		if req.Stdout != nil {
			streamWG.Add(1)
			go func() {
				defer streamWG.Done()
				for i := 0; i < 8; i++ {
					_, _ = io.WriteString(req.Stdout, "stdout line\n")
				}
			}()
		}
		if req.Stderr != nil {
			streamWG.Add(1)
			go func() {
				defer streamWG.Done()
				for i := 0; i < 8; i++ {
					_, _ = io.WriteString(req.Stderr, "#1 DONE\n")
				}
			}()
		}
		streamWG.Wait()
		// Record digest for the requested metadata file.
		for i := 0; i+1 < len(req.Command); i++ {
			if req.Command[i] == "--metadata-file" {
				metaPath := req.Command[i+1]
				f.mu.Lock()
				if f.metaJSON == nil {
					f.metaJSON = make(map[string]string)
				}
				f.metaJSON[metaPath] = `{"containerimage.digest":"sha256:deadbeef0123456789"}`
				f.mu.Unlock()
			}
		}
		return nil
	}
	if len(req.Command) == 2 && req.Command[0] == "cat" {
		f.mu.Lock()
		val := f.metaJSON[req.Command[1]]
		f.mu.Unlock()
		if req.Stdout != nil {
			_, _ = io.WriteString(req.Stdout, val)
		}
		return nil
	}
	return nil
}

func TestBuildKitRunner_RunAutoDetectsACRAndMirrorsSecret(t *testing.T) {
	osbDir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(osbDir, "server"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(osbDir, "server/Dockerfile"), []byte("FROM python:3.10-slim\n"), 0o644))

	fsbDir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(fsbDir, "build"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(fsbDir, "build/Dockerfile.oci-tools"), []byte("FROM ubuntu:22.04\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(fsbDir, "build/Dockerfile.firecracker-runtime"), []byte("FROM ubuntu:22.04\n"), 0o644))

	dockerCfg, err := json.Marshal(map[string]any{
		"auths": map[string]any{
			"opensandbox-registry-vpc.ap-southeast-1.cr.aliyuncs.com": map[string]any{
				"auth": "dGVzdDp0ZXN0",
			},
		},
	})
	require.NoError(t, err)

	cs := fake.NewSimpleClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "opensandbox-system"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "opensandbox-bench-run"}},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      DefaultRegistrySecretName,
				Namespace: "opensandbox-system",
			},
			Type: corev1.SecretTypeDockerConfigJson,
			Data: map[string][]byte{
				corev1.DockerConfigJsonKey: dockerCfg,
			},
		},
	)

	// Mark newly created BuildKit pod Running asynchronously so waitBuildKitReady completes.
	stopWatch := make(chan struct{})
	defer close(stopWatch)
	go func() {
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stopWatch:
				return
			case <-ticker.C:
				pods, err := cs.CoreV1().Pods("opensandbox-bench-run").List(context.Background(), metav1.ListOptions{})
				if err != nil {
					continue
				}
				for i := range pods.Items {
					p := &pods.Items[i]
					if p.Status.Phase != corev1.PodRunning {
						p.Status.Phase = corev1.PodRunning
						p.Spec.NodeName = "ack-x86-node-1"
						_, _ = cs.CoreV1().Pods("opensandbox-bench-run").UpdateStatus(context.Background(), p, metav1.UpdateOptions{})
					}
				}
			}
		}
	}()

	execer := &fakeBuildExecer{}
	var prebuiltPkgs []string
	bk := &BuildKitRunner{
		Clientset:    cs,
		Execer:       execer,
		PollInterval: 15 * time.Millisecond,
		ReadyTimeout: 5 * time.Second,
		PrebuildFunc: func(_ context.Context, _, pkg, _ string) (VirtualFile, error) {
			prebuiltPkgs = append(prebuiltPkgs, pkg)
			return VirtualFile{ArchivePath: ".build/linux-amd64/" + pkg, Data: []byte("BIN"), Mode: 0o755}, nil
		},
	}

	var progressBuf bytes.Buffer
	manifest, err := bk.Run(context.Background(), Options{
		Namespace:      "opensandbox-bench-run",
		Tag:            "test-v1",
		Components:     []string{"server", "fsb-firecracker-runtime"},
		OpenSandboxDir: osbDir,
		FastSandboxDir: fsbDir,
		Concurrency:    2,
		ProgressWriter: &progressBuf,
	})
	require.NoError(t, err)
	require.Equal(t, "opensandbox-registry-vpc.ap-southeast-1.cr.aliyuncs.com", manifest.Registry)
	require.Equal(t, "linux/amd64", manifest.Platform)
	require.Equal(t, []string{"firecracker-runtime"}, prebuiltPkgs)
	require.Len(t, manifest.Images, 3) // oci-tools + server + fsb-firecracker-runtime
	require.Equal(t, "oci-tools", manifest.Images[0].Component)
	require.Equal(t, "opensandbox-registry-vpc.ap-southeast-1.cr.aliyuncs.com/opensandbox/oci-tools:test-v1", manifest.Images[0].Image)
	require.Equal(t, "sha256:deadbeef0123456789", manifest.Images[0].Digest)
	require.Equal(t, 2, execer.uploads)

	// Verify oci-tools build command ran BEFORE fsb-firecracker-runtime with OCI_TOOLS_IMAGE arg.
	var ociIdx, rtIdx int
	ociIdx, rtIdx = -1, -1
	for i, cmd := range execer.commands {
		joined := strings.Join(cmd, " ")
		if strings.Contains(joined, "filename=Dockerfile.oci-tools") {
			ociIdx = i
		}
		if strings.Contains(joined, "filename=Dockerfile.firecracker-runtime") {
			rtIdx = i
			require.Contains(t, joined, "build-arg:OCI_TOOLS_IMAGE=opensandbox-registry-vpc.ap-southeast-1.cr.aliyuncs.com/opensandbox/oci-tools:test-v1")
			require.Contains(t, joined, "platform=linux/amd64")
			require.Contains(t, joined, "push=true")
		}
	}
	require.GreaterOrEqual(t, ociIdx, 0)
	require.Greater(t, rtIdx, ociIdx)

	// Verify temporary BuildKit pod and mirrored secret were cleaned up.
	pods, err := cs.CoreV1().Pods("opensandbox-bench-run").List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	require.Empty(t, pods.Items)
	secrets, err := cs.CoreV1().Secrets("opensandbox-bench-run").List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	require.Empty(t, secrets.Items)

	// Verify Helm overrides generation enables optional charts.
	overrides := BuildHelmOverrides(manifest.Registry, manifest.RepoPrefix, manifest.Tag, manifest.Images)
	require.Contains(t, overrides, "opensandbox-server")
	require.Contains(t, overrides, "fast-sandbox")
	fsbMap := overrides["fast-sandbox"].(map[string]any)
	require.Equal(t, true, fsbMap["enabled"])

	ingressOverrides := BuildHelmOverrides(manifest.Registry, manifest.RepoPrefix, manifest.Tag, []ComponentBuildResult{
		{Component: "ingress", Image: manifest.Registry + "/opensandbox/ingress:" + manifest.Tag},
	})
	igMap := ingressOverrides["ingress-gateway"].(map[string]any)
	require.Equal(t, true, igMap["enabled"])
}

func TestBuildKitRunner_PlainHTTPRegistryAutoConfig(t *testing.T) {
	osbDir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(osbDir, "server"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(osbDir, "server/Dockerfile"), []byte("FROM python:3.10-slim\n"), 0o644))

	dockerCfg, err := json.Marshal(map[string]any{
		"auths": map[string]any{
			"opensandbox-bench-registry.opensandbox-bench-run.svc:5000": map[string]any{"auth": "dGVzdDp0ZXN0"},
		},
	})
	require.NoError(t, err)

	cs := fake.NewSimpleClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "opensandbox-bench-run"}},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: DefaultRegistrySecretName, Namespace: "opensandbox-bench-run"},
			Type:       corev1.SecretTypeDockerConfigJson,
			Data:       map[string][]byte{corev1.DockerConfigJsonKey: dockerCfg},
		},
	)

	var seenPodArgs string
	stopWatch := make(chan struct{})
	defer close(stopWatch)
	go func() {
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stopWatch:
				return
			case <-ticker.C:
				pods, err := cs.CoreV1().Pods("opensandbox-bench-run").List(context.Background(), metav1.ListOptions{})
				if err != nil {
					continue
				}
				for i := range pods.Items {
					p := &pods.Items[i]
					if len(p.Spec.Containers) > 0 && seenPodArgs == "" {
						seenPodArgs = strings.Join(p.Spec.Containers[0].Args, " ")
					}
					if p.Status.Phase != corev1.PodRunning {
						p.Status.Phase = corev1.PodRunning
						_, _ = cs.CoreV1().Pods("opensandbox-bench-run").UpdateStatus(context.Background(), p, metav1.UpdateOptions{})
					}
				}
			}
		}
	}()

	execer := &fakeBuildExecer{}
	bk := &BuildKitRunner{Clientset: cs, Execer: execer, PollInterval: 15 * time.Millisecond, ReadyTimeout: 5 * time.Second}
	_, err = bk.Run(context.Background(), Options{
		Namespace:      "opensandbox-bench-run",
		Tag:            "test-http",
		Components:     []string{"server"},
		OpenSandboxDir: osbDir,
	})
	require.NoError(t, err)
	require.Contains(t, seenPodArgs, "http = true")

	foundInsecure := false
	for _, cmd := range execer.commands {
		if strings.Contains(strings.Join(cmd, " "), "registry.insecure=true") {
			foundInsecure = true
		}
	}
	require.True(t, foundInsecure, "plain-HTTP registry must pass registry.insecure=true to buildctl")
}

func TestBuildKitRunner_RejectsNonAMD64ForFastSandbox(t *testing.T) {
	bk := &BuildKitRunner{
		Clientset: fake.NewSimpleClientset(),
		Execer:    &fakeBuildExecer{},
	}
	_, err := bk.Run(context.Background(), Options{
		Components:     []string{"fsb-controller"},
		FastSandboxDir: t.TempDir(),
		Platform:       "linux/arm64",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "linux/amd64")
}

func TestDefaultAutoTag_CoversBothReposAndDirtyState(t *testing.T) {
	now := time.Date(2026, 11, 28, 5, 30, 0, 0, time.UTC)
	require.Equal(t, "bench-osb-11111111-fsb-22222222", defaultAutoTag(true, "11111111aa", false, true, "22222222bb", false, now))
	require.Equal(t, "bench-osb-11111111-fsb-22222222-dirty-20261128053000", defaultAutoTag(true, "11111111aa", false, true, "22222222bb", true, now))
	require.Equal(t, "bench-fsb-22222222", defaultAutoTag(false, "", false, true, "22222222bb", false, now))
}

func TestBuildKitRunner_NoPushRejectsOCIToolsDependentsWithoutOverride(t *testing.T) {
	bk := &BuildKitRunner{
		Clientset: fake.NewSimpleClientset(),
		Execer:    &fakeBuildExecer{},
	}
	_, err := bk.Run(context.Background(), Options{
		Components:     []string{"fsb-firecracker-runtime"},
		FastSandboxDir: t.TempDir(),
		NoPush:         true,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "--no-push")
	require.Contains(t, err.Error(), "--oci-tools-image")
}

func TestPEP440VersionFromTag(t *testing.T) {
	require.Equal(t, "1.3.0rc4", pep440VersionFromTag("release-1.3.0-rc.4"))
	require.Equal(t, "1.3.0rc5", pep440VersionFromTag("v1.3.0-rc.5"))
	require.Equal(t, "1.3.0", pep440VersionFromTag("v1.3.0"))
	require.Equal(t, "0.2.2", pep440VersionFromTag("0.2.2"))
	require.Equal(t, "", pep440VersionFromTag("bench-6817b13ee"))
	require.Equal(t, "", pep440VersionFromTag("dev"))
}
