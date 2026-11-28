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
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/runner"
)

const (
	buildkitContainerName = "buildkitd"
	buildkitSocketAddr    = "unix:///run/buildkit/buildkitd.sock"
	buildkitWorkspaceRoot = "/workspace"
	buildkitManagedLabel  = "bench.opensandbox.io/managed-by"
	buildkitManagedValue  = "opensandbox-bench-build"
)

// Options configures an in-cluster BuildKit full-stack image build and push run.
type Options struct {
	Namespace               string
	Registry                string
	RepoPrefix              string
	Tag                     string
	Platform                string
	Components              []string
	OpenSandboxDir          string
	FastSandboxDir          string
	RegistrySecretName      string
	RegistrySecretNamespace string
	BuildKitImage           string
	CacheHostPath           string
	OCIToolsImageOverride   string
	GoProxy                 string
	PipIndexURL             string
	Concurrency             int
	KeepBuilder             bool
	NoPush                  bool
	PlainHTTP               bool
	ProgressWriter          io.Writer
}

// BuildKitRunner orchestrates a temporary in-cluster BuildKit pod on an x86_64/amd64
// node to compile and push full-stack OpenSandbox + fast-sandbox images to ACR or any
// OCI registry authenticated by an existing Kubernetes dockerconfigjson secret.
type BuildKitRunner struct {
	Clientset    kubernetes.Interface
	Execer       runner.PodExecer
	Logger       *slog.Logger
	PrebuildFunc func(ctx context.Context, fsbDir, pkg, goProxy string) (VirtualFile, error)
	PollInterval time.Duration
	ReadyTimeout time.Duration
}

// NewBuildKitRunner constructs a BuildKitRunner backed by client-go and SPDY exec.
func NewBuildKitRunner(kc *runner.KubeClients, log *slog.Logger) *BuildKitRunner {
	if log == nil {
		log = slog.Default()
	}
	return &BuildKitRunner{
		Clientset: kc.Clientset,
		Execer: &runner.SPDYPodExecer{
			RestConfig: kc.RestConfig,
			Clientset:  kc.Clientset,
		},
		Logger:       log,
		PrebuildFunc: PrebuildFastSandboxBinary,
		PollInterval: 2 * time.Second,
		ReadyTimeout: 5 * time.Minute,
	}
}

func (b *BuildKitRunner) pollInterval() time.Duration {
	if b.PollInterval > 0 {
		return b.PollInterval
	}
	return 2 * time.Second
}

func (b *BuildKitRunner) readyTimeout() time.Duration {
	if b.ReadyTimeout > 0 {
		return b.ReadyTimeout
	}
	return 5 * time.Minute
}

func (b *BuildKitRunner) logger() *slog.Logger {
	if b.Logger != nil {
		return b.Logger
	}
	return slog.Default()
}

// Run executes the complete build plan in-cluster and returns the build manifest.
func (b *BuildKitRunner) Run(ctx context.Context, opts Options) (*BuildManifest, error) {
	if b.Clientset == nil || b.Execer == nil {
		return nil, fmt.Errorf("BuildKitRunner requires non-nil Clientset and Execer")
	}

	if strings.TrimSpace(opts.Namespace) == "" {
		opts.Namespace = "opensandbox-bench-run"
	}
	if strings.TrimSpace(opts.RepoPrefix) == "" {
		opts.RepoPrefix = DefaultRepoPrefix
	}
	if strings.TrimSpace(opts.Platform) == "" {
		opts.Platform = DefaultPlatform
	}
	if strings.TrimSpace(opts.BuildKitImage) == "" {
		opts.BuildKitImage = DefaultBuildKitImage
	}
	if strings.TrimSpace(opts.RegistrySecretName) == "" {
		opts.RegistrySecretName = DefaultRegistrySecretName
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = 2
	}
	if opts.ProgressWriter != nil {
		opts.ProgressWriter = &lockedWriter{w: opts.ProgressWriter}
	}

	plan, err := ResolveComponents(opts.Components, opts.OCIToolsImageOverride)
	if err != nil {
		return nil, err
	}

	if opts.NoPush && strings.TrimSpace(opts.OCIToolsImageOverride) == "" {
		for _, c := range plan {
			if c.NeedsOCITools {
				return nil, fmt.Errorf("cannot build %s with --no-push without --oci-tools-image (its Dockerfile pulls FROM ${OCI_TOOLS_IMAGE} from the registry)", c.Name)
			}
		}
	}

	if opts.Platform != DefaultPlatform {
		for _, c := range plan {
			if c.PrebuildBinary != "" || c.Repo == RepoFastSandbox {
				return nil, fmt.Errorf("fast-sandbox component %q only supports --platform %s (got %q)", c.Name, DefaultPlatform, opts.Platform)
			}
		}
	}

	needsOSB, needsFSB := false, false
	for _, c := range plan {
		if c.Repo == RepoOpenSandbox {
			needsOSB = true
		}
		if c.Repo == RepoFastSandbox {
			needsFSB = true
		}
	}
	if needsOSB && strings.TrimSpace(opts.OpenSandboxDir) == "" {
		return nil, fmt.Errorf("selected OpenSandbox components require --opensandbox-dir")
	}
	if needsFSB && strings.TrimSpace(opts.FastSandboxDir) == "" {
		return nil, fmt.Errorf("selected fast-sandbox components require --fast-sandbox-dir")
	}

	osbCommit := GitShortCommit(ctx, opts.OpenSandboxDir)
	fsbCommit := GitShortCommit(ctx, opts.FastSandboxDir)
	if strings.TrimSpace(opts.Tag) == "" {
		osbDirty := needsOSB && GitIsDirty(ctx, opts.OpenSandboxDir)
		fsbDirty := needsFSB && GitIsDirty(ctx, opts.FastSandboxDir)
		opts.Tag = defaultAutoTag(needsOSB, osbCommit, osbDirty, needsFSB, fsbCommit, fsbDirty, time.Now().UTC())
	}

	// Ensure target namespace exists.
	if err := b.ensureNamespace(ctx, opts.Namespace); err != nil {
		return nil, err
	}

	// Locate and optionally mirror the cluster's dockerconfigjson ACR secret.
	secretData, secretSrcNS, err := b.findRegistrySecret(ctx, opts.RegistrySecretName, opts.RegistrySecretNamespace, opts.Namespace)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(opts.Registry) == "" {
		hosts := extractDockerConfigHosts(secretData)
		if len(hosts) == 0 {
			return nil, fmt.Errorf("--registry is required (no registry hosts found in secret %s/%s)", secretSrcNS, opts.RegistrySecretName)
		}
		opts.Registry = hosts[0]
		b.logger().Info("auto-detected target registry from cluster secret", "registry", opts.Registry, "secret", secretSrcNS+"/"+opts.RegistrySecretName)
	}
	if !opts.PlainHTTP && isLikelyPlainHTTPRegistry(opts.Registry) {
		opts.PlainHTTP = true
		b.logger().Info("enabled plain-HTTP BuildKit registry mode for in-cluster/local registry", "registry", opts.Registry)
	}

	suffix := shortRandHex(4)
	mountedSecretName := opts.RegistrySecretName
	var cleanupSecretName string
	if secretSrcNS != opts.Namespace {
		mountedSecretName = fmt.Sprintf("opensandbox-bench-buildkit-reg-%s", suffix)
		if err := b.createMirroredSecret(ctx, opts.Namespace, mountedSecretName, secretData); err != nil {
			return nil, err
		}
		cleanupSecretName = mountedSecretName
	}

	podName := fmt.Sprintf("opensandbox-bench-buildkit-%s", suffix)
	if !opts.KeepBuilder {
		defer func() {
			cleanCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if delErr := b.Clientset.CoreV1().Pods(opts.Namespace).Delete(cleanCtx, podName, metav1.DeleteOptions{}); delErr != nil && !apierrors.IsNotFound(delErr) {
				b.logger().Warn("failed to delete temporary buildkit pod", "pod", podName, "error", delErr)
			}
			if cleanupSecretName != "" {
				if delErr := b.Clientset.CoreV1().Secrets(opts.Namespace).Delete(cleanCtx, cleanupSecretName, metav1.DeleteOptions{}); delErr != nil && !apierrors.IsNotFound(delErr) {
					b.logger().Warn("failed to delete mirrored registry secret", "secret", cleanupSecretName, "error", delErr)
				}
			}
		}()
	}

	// Pre-compile static linux/amd64 Go binaries for fast-sandbox components whose
	// Dockerfiles copy .build/linux-amd64/<binary> before packaging the context tar.
	var fsbExtraFiles []VirtualFile
	if needsFSB {
		prebuildFn := b.PrebuildFunc
		if prebuildFn == nil {
			prebuildFn = PrebuildFastSandboxBinary
		}
		prebuilt := make(map[string]bool)
		for _, c := range plan {
			if c.Repo == RepoFastSandbox && c.PrebuildBinary != "" && !prebuilt[c.PrebuildBinary] {
				prebuilt[c.PrebuildBinary] = true
				b.logger().Info("cross-compiling fast-sandbox linux/amd64 binary", "binary", c.PrebuildBinary)
				vf, err := prebuildFn(ctx, opts.FastSandboxDir, c.PrebuildBinary, opts.GoProxy)
				if err != nil {
					return nil, err
				}
				fsbExtraFiles = append(fsbExtraFiles, vf)
			}
		}
	}

	// Create and wait for the in-cluster BuildKit pod on an x86_64/amd64 node.
	if err := b.createBuildKitPod(ctx, opts, podName, mountedSecretName); err != nil {
		return nil, err
	}
	if err := b.waitBuildKitReady(ctx, opts.Namespace, podName); err != nil {
		return nil, err
	}

	// Upload source contexts once per required repository.
	if needsOSB {
		b.logger().Info("packaging and uploading OpenSandbox build context", "dir", opts.OpenSandboxDir)
		tarGz, err := ArchiveRepoContext(opts.OpenSandboxDir, RepoOpenSandbox, plan, nil)
		if err != nil {
			return nil, err
		}
		if err := b.uploadTarGz(ctx, opts.Namespace, podName, path.Join(buildkitWorkspaceRoot, string(RepoOpenSandbox)), tarGz); err != nil {
			return nil, fmt.Errorf("upload OpenSandbox context: %w", err)
		}
	}
	if needsFSB {
		b.logger().Info("packaging and uploading fast-sandbox build context", "dir", opts.FastSandboxDir, "prebuilt_binaries", len(fsbExtraFiles))
		tarGz, err := ArchiveRepoContext(opts.FastSandboxDir, RepoFastSandbox, plan, fsbExtraFiles)
		if err != nil {
			return nil, err
		}
		if err := b.uploadTarGz(ctx, opts.Namespace, podName, path.Join(buildkitWorkspaceRoot, string(RepoFastSandbox)), tarGz); err != nil {
			return nil, fmt.Errorf("upload fast-sandbox context: %w", err)
		}
	}

	ociToolsRef := strings.TrimSpace(opts.OCIToolsImageOverride)
	if ociToolsRef == "" {
		ociToolsRef = FullImageRef(opts.Registry, opts.RepoPrefix, "oci-tools", opts.Tag)
	}

	// Stage 1: build oci-tools first if present (fsb-firecracker-runtime and
	// fsb-sandboxtemplate-builder pull it via FROM ${OCI_TOOLS_IMAGE}).
	var stage1, stage2 []ComponentSpec
	for _, c := range plan {
		if c.Name == "oci-tools" {
			stage1 = append(stage1, c)
		} else {
			stage2 = append(stage2, c)
		}
	}

	resultsByName := make(map[string]ComponentBuildResult, len(plan))
	var resultsMu sync.Mutex

	runOne := func(c ComponentSpec) error {
		res, err := b.buildComponent(ctx, opts, podName, c, ociToolsRef, osbCommit, fsbCommit)
		if err != nil {
			return err
		}
		resultsMu.Lock()
		resultsByName[c.Name] = res
		resultsMu.Unlock()
		return nil
	}

	for _, c := range stage1 {
		if err := runOne(c); err != nil {
			return nil, err
		}
	}

	if len(stage2) > 0 {
		stageCtx, cancelStage := context.WithCancel(ctx)
		defer cancelStage()

		sem := make(chan struct{}, opts.Concurrency)
		var wg sync.WaitGroup
		var firstErr error
		var errMu sync.Mutex

		for _, comp := range stage2 {
			c := comp
			wg.Add(1)
			go func() {
				defer wg.Done()
				select {
				case sem <- struct{}{}:
				case <-stageCtx.Done():
					return
				}
				defer func() { <-sem }()
				if stageCtx.Err() != nil {
					return
				}
				res, err := b.buildComponent(stageCtx, opts, podName, c, ociToolsRef, osbCommit, fsbCommit)
				if err != nil {
					errMu.Lock()
					if firstErr == nil {
						firstErr = err
						cancelStage()
					}
					errMu.Unlock()
					return
				}
				resultsMu.Lock()
				resultsByName[c.Name] = res
				resultsMu.Unlock()
			}()
		}
		wg.Wait()
		if firstErr != nil {
			return nil, firstErr
		}
	}

	orderedResults := make([]ComponentBuildResult, 0, len(plan))
	for _, c := range plan {
		if r, ok := resultsByName[c.Name]; ok {
			orderedResults = append(orderedResults, r)
		}
	}

	return &BuildManifest{
		TimestampUTC:      time.Now().UTC().Format(time.RFC3339),
		Platform:          opts.Platform,
		Registry:          opts.Registry,
		RepoPrefix:        opts.RepoPrefix,
		Tag:               opts.Tag,
		OpenSandboxCommit: osbCommit,
		FastSandboxCommit: fsbCommit,
		Images:            orderedResults,
	}, nil
}

func (b *BuildKitRunner) ensureNamespace(ctx context.Context, ns string) error {
	_, err := b.Clientset.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get namespace %q: %w", ns, err)
	}
	_, err = b.Clientset.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   ns,
			Labels: map[string]string{buildkitManagedLabel: buildkitManagedValue},
		},
	}, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create namespace %q: %w", ns, err)
	}
	return nil
}

func (b *BuildKitRunner) findRegistrySecret(ctx context.Context, secretName, explicitNS, buildNS string) ([]byte, string, error) {
	candidates := make([]string, 0, 4)
	if strings.TrimSpace(explicitNS) != "" {
		candidates = append(candidates, strings.TrimSpace(explicitNS))
	} else {
		for _, ns := range []string{buildNS, "opensandbox-system", "opensandbox-dataplane", "default"} {
			seen := false
			for _, c := range candidates {
				if c == ns {
					seen = true
					break
				}
			}
			if !seen && ns != "" {
				candidates = append(candidates, ns)
			}
		}
	}

	for _, ns := range candidates {
		sec, err := b.Clientset.CoreV1().Secrets(ns).Get(ctx, secretName, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, "", fmt.Errorf("get registry secret %s/%s: %w", ns, secretName, err)
		}
		raw, ok := sec.Data[corev1.DockerConfigJsonKey]
		if !ok || len(raw) == 0 {
			return nil, "", fmt.Errorf("secret %s/%s is missing %s key", ns, secretName, corev1.DockerConfigJsonKey)
		}
		return raw, ns, nil
	}
	return nil, "", fmt.Errorf("registry secret %q not found in namespaces %v", secretName, candidates)
}

func (b *BuildKitRunner) createMirroredSecret(ctx context.Context, ns, name string, dockerConfigJSON []byte) error {
	_, err := b.Clientset.CoreV1().Secrets(ns).Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels:    map[string]string{buildkitManagedLabel: buildkitManagedValue},
		},
		Type: corev1.SecretTypeDockerConfigJson,
		Data: map[string][]byte{
			corev1.DockerConfigJsonKey: dockerConfigJSON,
		},
	}, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("mirror registry secret to %s/%s: %w", ns, name, err)
	}
	return nil
}

func (b *BuildKitRunner) createBuildKitPod(ctx context.Context, opts Options, podName, secretName string) error {
	privileged := true
	stateVol := corev1.VolumeSource{
		EmptyDir: &corev1.EmptyDirVolumeSource{},
	}
	if strings.TrimSpace(opts.CacheHostPath) != "" {
		hpType := corev1.HostPathDirectoryOrCreate
		stateVol = corev1.VolumeSource{
			HostPath: &corev1.HostPathVolumeSource{
				Path: strings.TrimSpace(opts.CacheHostPath),
				Type: &hpType,
			},
		}
	}

	var (
		containerCmd  []string
		containerArgs []string
	)
	if opts.PlainHTTP {
		regHost := strings.TrimRight(strings.TrimSpace(opts.Registry), "/")
		tomlCfg := fmt.Sprintf("[registry.%q]\n  http = true\n", regHost)
		containerCmd = []string{"sh", "-c"}
		containerArgs = []string{
			fmt.Sprintf("mkdir -p /etc/buildkit && printf '%%s' %q > /etc/buildkit/buildkitd.toml && exec buildkitd --addr %q --config /etc/buildkit/buildkitd.toml --oci-worker-gc-keepstorage 20000", tomlCfg, buildkitSocketAddr),
		}
	} else {
		containerArgs = []string{
			"--addr", buildkitSocketAddr,
			"--oci-worker-gc-keepstorage", "20000",
		}
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: opts.Namespace,
			Labels: map[string]string{
				"app":                "opensandbox-bench-buildkit",
				buildkitManagedLabel: buildkitManagedValue,
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			ImagePullSecrets: []corev1.LocalObjectReference{
				{Name: secretName},
			},
			NodeSelector: map[string]string{
				"kubernetes.io/arch": "amd64",
			},
			Affinity: &corev1.Affinity{
				NodeAffinity: &corev1.NodeAffinity{
					PreferredDuringSchedulingIgnoredDuringExecution: []corev1.PreferredSchedulingTerm{
						{
							Weight: 50,
							Preference: corev1.NodeSelectorTerm{
								MatchExpressions: []corev1.NodeSelectorRequirement{
									{
										Key:      runner.FirecrackerNodeLabelKey,
										Operator: corev1.NodeSelectorOpNotIn,
										Values:   []string{"true"},
									},
								},
							},
						},
					},
				},
			},
			Containers: []corev1.Container{
				{
					Name:            buildkitContainerName,
					Image:           opts.BuildKitImage,
					ImagePullPolicy: corev1.PullIfNotPresent,
					Command:         containerCmd,
					Args:            containerArgs,
					SecurityContext: &corev1.SecurityContext{
						Privileged: &privileged,
					},
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("4"),
							corev1.ResourceMemory: resource.MustParse("8Gi"),
						},
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("32"),
							corev1.ResourceMemory: resource.MustParse("32Gi"),
						},
					},
					VolumeMounts: []corev1.VolumeMount{
						{Name: "docker-config", MountPath: "/root/.docker", ReadOnly: true},
						{Name: "buildkit-state", MountPath: "/var/lib/buildkit"},
						{Name: "workspace", MountPath: buildkitWorkspaceRoot},
					},
				},
			},
			Volumes: []corev1.Volume{
				{
					Name: "docker-config",
					VolumeSource: corev1.VolumeSource{
						Secret: &corev1.SecretVolumeSource{
							SecretName: secretName,
							Items: []corev1.KeyToPath{
								{Key: corev1.DockerConfigJsonKey, Path: "config.json"},
							},
						},
					},
				},
				{
					Name:         "buildkit-state",
					VolumeSource: stateVol,
				},
				{
					Name: "workspace",
					VolumeSource: corev1.VolumeSource{
						EmptyDir: &corev1.EmptyDirVolumeSource{},
					},
				},
			},
		},
	}

	b.logger().Info("creating in-cluster BuildKit pod", "namespace", opts.Namespace, "pod", podName, "image", opts.BuildKitImage, "platform", opts.Platform)
	if _, err := b.Clientset.CoreV1().Pods(opts.Namespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create buildkit pod %s/%s: %w", opts.Namespace, podName, err)
	}
	return nil
}

func (b *BuildKitRunner) waitBuildKitReady(ctx context.Context, ns, podName string) error {
	waitCtx, cancel := context.WithTimeout(ctx, b.readyTimeout())
	defer cancel()

	ticker := time.NewTicker(b.pollInterval())
	defer ticker.Stop()

	for {
		pod, err := b.Clientset.CoreV1().Pods(ns).Get(waitCtx, podName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("get buildkit pod %s/%s: %w", ns, podName, err)
		}
		if pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded {
			return fmt.Errorf("buildkit pod %s/%s exited prematurely (phase=%s)", ns, podName, pod.Status.Phase)
		}
		if pod.Status.Phase == corev1.PodRunning {
			var stdout, stderr bytes.Buffer
			execErr := b.Execer.Exec(waitCtx, runner.ExecRequest{
				Namespace: ns,
				PodName:   podName,
				Container: buildkitContainerName,
				Command:   []string{"buildctl", "--addr", buildkitSocketAddr, "debug", "workers"},
				Stdout:    &stdout,
				Stderr:    &stderr,
			})
			if execErr == nil {
				b.logger().Info("in-cluster BuildKit daemon ready", "pod", podName, "node", pod.Spec.NodeName)
				return nil
			}
		}
		select {
		case <-waitCtx.Done():
			return fmt.Errorf("timed out waiting for buildkit pod %s/%s readiness: %w", ns, podName, waitCtx.Err())
		case <-ticker.C:
		}
	}
}

func (b *BuildKitRunner) uploadTarGz(ctx context.Context, ns, podName, remoteDir string, tarGz []byte) error {
	var stderr bytes.Buffer
	cmd := []string{"sh", "-c", fmt.Sprintf("mkdir -p %q && tar -xzf - -C %q", remoteDir, remoteDir)}
	if err := b.Execer.Exec(ctx, runner.ExecRequest{
		Namespace: ns,
		PodName:   podName,
		Container: buildkitContainerName,
		Command:   cmd,
		Stdin:     bytes.NewReader(tarGz),
		Stderr:    &stderr,
	}); err != nil {
		return fmt.Errorf("extract context into %s: %w (stderr: %s)", remoteDir, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func (b *BuildKitRunner) buildComponent(ctx context.Context, opts Options, podName string, c ComponentSpec, ociToolsRef, osbCommit, fsbCommit string) (ComponentBuildResult, error) {
	imageRef := FullImageRef(opts.Registry, opts.RepoPrefix, c.Name, opts.Tag)
	repoRoot := path.Join(buildkitWorkspaceRoot, string(c.Repo))
	contextPath := repoRoot
	if c.ContextDir != "" && c.ContextDir != "." {
		contextPath = path.Join(repoRoot, c.ContextDir)
	}
	dockerfileFull := path.Join(repoRoot, c.Dockerfile)
	dockerfileDir := path.Dir(dockerfileFull)
	dockerfileName := path.Base(dockerfileFull)
	metaFile := path.Join(buildkitWorkspaceRoot, fmt.Sprintf("meta-%s.json", c.Name))

	args := []string{
		"buildctl", "--addr", buildkitSocketAddr, "build",
		"--frontend", "dockerfile.v0",
		"--local", "context=" + contextPath,
		"--local", "dockerfile=" + dockerfileDir,
		"--opt", "filename=" + dockerfileName,
		"--opt", "platform=" + opts.Platform,
		"--progress", "plain",
		"--metadata-file", metaFile,
	}

	if c.NeedsOCITools && ociToolsRef != "" {
		args = append(args, "--opt", "build-arg:OCI_TOOLS_IMAGE="+ociToolsRef)
	}
	if c.SupportsGoProxy && strings.TrimSpace(opts.GoProxy) != "" {
		args = append(args, "--opt", "build-arg:GOPROXY="+strings.TrimSpace(opts.GoProxy))
	}
	if c.SupportsPipIndex && strings.TrimSpace(opts.PipIndexURL) != "" {
		args = append(args, "--opt", "build-arg:PIP_INDEX_URL="+strings.TrimSpace(opts.PipIndexURL))
	}
	if c.NeedsVersionArgs {
		commit := osbCommit
		if c.Repo == RepoFastSandbox {
			commit = fsbCommit
		}
		args = append(args,
			"--opt", "build-arg:VERSION="+opts.Tag,
			"--opt", "build-arg:GIT_COMMIT="+commit,
			"--opt", "build-arg:BUILD_TIME="+time.Now().UTC().Format(time.RFC3339),
		)
	}

	if c.NeedsPythonVersionArg {
		if pepVer := pep440VersionFromTag(opts.Tag); pepVer != "" {
			args = append(args, "--opt", "build-arg:SETUPTOOLS_SCM_PRETEND_VERSION="+pepVer)
		}
	}

	pushFlag := "true"
	if opts.NoPush {
		pushFlag = "false"
	}
	outputSpec := fmt.Sprintf("type=image,name=%s,push=%s", imageRef, pushFlag)
	if opts.PlainHTTP {
		outputSpec += ",registry.insecure=true"
	}
	args = append(args, "--output", outputSpec)

	b.logger().Info("building image in-cluster with BuildKit", "component", c.Name, "image", imageRef, "push", !opts.NoPush)
	start := time.Now()
	var stderrBuf bytes.Buffer
	// SPDY remotecommand copies stdout and stderr on separate goroutines; wrap
	// stderrBuf in a mutex-protected writer to avoid concurrent buffer races.
	bufW := &lockedWriter{w: &stderrBuf}
	var stdoutW, stderrW io.Writer = bufW, bufW
	if opts.ProgressWriter != nil {
		stdoutW = io.MultiWriter(opts.ProgressWriter, bufW)
		stderrW = io.MultiWriter(opts.ProgressWriter, bufW)
	}

	if err := b.Execer.Exec(ctx, runner.ExecRequest{
		Namespace: opts.Namespace,
		PodName:   podName,
		Container: buildkitContainerName,
		Command:   args,
		Stdout:    stdoutW,
		Stderr:    stderrW,
	}); err != nil {
		tail := stderrBuf.String()
		if len(tail) > 2000 {
			tail = tail[len(tail)-2000:]
		}
		return ComponentBuildResult{}, fmt.Errorf("buildctl build %s (%s): %w\n%s", c.Name, imageRef, err, strings.TrimSpace(tail))
	}
	dur := time.Since(start)

	digest := b.readMetadataDigest(ctx, opts.Namespace, podName, metaFile)
	b.logger().Info("built image", "component", c.Name, "image", imageRef, "digest", digest, "duration", dur.Round(time.Millisecond))

	return ComponentBuildResult{
		Component:       c.Name,
		Repo:            c.Repo,
		Image:           imageRef,
		Digest:          digest,
		Platform:        opts.Platform,
		DurationSeconds: dur.Seconds(),
		Duration:        dur,
	}, nil
}

func (b *BuildKitRunner) readMetadataDigest(ctx context.Context, ns, podName, metaFile string) string {
	var out bytes.Buffer
	if err := b.Execer.Exec(ctx, runner.ExecRequest{
		Namespace: ns,
		PodName:   podName,
		Container: buildkitContainerName,
		Command:   []string{"cat", metaFile},
		Stdout:    &out,
	}); err != nil {
		return ""
	}
	var parsed map[string]any
	if err := json.Unmarshal(out.Bytes(), &parsed); err != nil {
		return ""
	}
	if d, ok := parsed["containerimage.digest"].(string); ok {
		return strings.TrimSpace(d)
	}
	return ""
}

// extractDockerConfigHosts extracts registry hostnames from a .dockerconfigjson
// payload without exposing any credentials.
func extractDockerConfigHosts(raw []byte) []string {
	var parsed struct {
		Auths map[string]json.RawMessage `json:"auths"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil || len(parsed.Auths) == 0 {
		return nil
	}
	hosts := make([]string, 0, len(parsed.Auths))
	for host := range parsed.Auths {
		h := strings.TrimSpace(host)
		h = strings.TrimPrefix(h, "https://")
		h = strings.TrimPrefix(h, "http://")
		h = strings.TrimRight(h, "/")
		if h != "" {
			hosts = append(hosts, h)
		}
	}
	sort.Strings(hosts)
	return hosts
}

func shortRandHex(nBytes int) string {
	buf := make([]byte, nBytes)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano()%100000)
	}
	return hex.EncodeToString(buf)
}

type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// pep440VersionFromTag converts release-style tags (e.g. "v1.3.0-rc.4",
// "release-1.3.0-rc.4", "1.3.0") into PEP 440 versions accepted by hatch-vcs /
// setuptools-scm. Arbitrary dev tags (e.g. "bench-6817b13ee") return "" so
// server/Dockerfile falls back safely to its built-in fallback_version.
func pep440VersionFromTag(tag string) string {
	s := strings.TrimSpace(tag)
	s = strings.TrimPrefix(s, "release-")
	s = strings.TrimPrefix(s, "v")
	if s == "" || s[0] < '0' || s[0] > '9' {
		return ""
	}
	// Normalize -rc.N / -alpha.N / -beta.N into PEP 440 pre-release suffixes.
	replacer := strings.NewReplacer(
		"-rc.", "rc",
		"-rc", "rc",
		"-alpha.", "a",
		"-beta.", "b",
	)
	s = replacer.Replace(s)
	for i := 0; i < len(s); i++ {
		c := s[i]
		isDigit := c >= '0' && c <= '9'
		isAllowed := isDigit || c == '.' || c == 'a' || c == 'b' || c == 'r' || c == 'c'
		if !isAllowed {
			return ""
		}
	}
	if !strings.Contains(s, ".") {
		return ""
	}
	return s
}

func defaultAutoTag(needsOSB bool, osbCommit string, osbDirty bool, needsFSB bool, fsbCommit string, fsbDirty bool, now time.Time) string {
	var parts []string
	if needsOSB && osbCommit != "" {
		s := osbCommit
		if len(s) > 8 {
			s = s[:8]
		}
		parts = append(parts, "osb-"+s)
	}
	if needsFSB && fsbCommit != "" {
		s := fsbCommit
		if len(s) > 8 {
			s = s[:8]
		}
		parts = append(parts, "fsb-"+s)
	}
	if len(parts) == 0 {
		return "bench-" + now.Format("20060102-150405")
	}
	tag := "bench-" + strings.Join(parts, "-")
	if osbDirty || fsbDirty {
		tag += "-dirty-" + now.Format("20060102150405")
	}
	return tag
}

func isLikelyPlainHTTPRegistry(host string) bool {
	h := strings.ToLower(strings.TrimRight(strings.TrimSpace(host), "/"))
	if strings.HasPrefix(h, "http://") {
		return true
	}
	h = strings.TrimPrefix(h, "https://")
	return strings.HasPrefix(h, "localhost:") ||
		strings.HasPrefix(h, "127.0.0.1:") ||
		strings.HasSuffix(h, ".svc:5000") ||
		strings.HasSuffix(h, ".svc.cluster.local:5000") ||
		strings.Contains(h, "opensandbox-bench-registry")
}
