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
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/profile"
)

// KubeClients bundles the client-go Clientset and underlying rest.Config
// (needed for SPDY pods/exec binary injection and bundle tar streaming).
type KubeClients struct {
	RestConfig *rest.Config
	Clientset  kubernetes.Interface
}

// LoadKubeClients builds a Kubernetes client from kubeconfig (or in-cluster config when empty).
func LoadKubeClients(kubeconfig string) (*KubeClients, error) {
	var (
		cfg *rest.Config
		err error
	)
	if kubeconfig != "" {
		cfg, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	} else if envKC := os.Getenv("KUBECONFIG"); envKC != "" {
		cfg, err = clientcmd.BuildConfigFromFlags("", envKC)
	} else {
		cfg, err = rest.InClusterConfig()
		if err != nil {
			home, _ := os.UserHomeDir()
			defaultKC := filepath.Join(home, ".kube", "config")
			cfg, err = clientcmd.BuildConfigFromFlags("", defaultKC)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig: %w", err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("create kubernetes clientset: %w", err)
	}
	return &KubeClients{RestConfig: cfg, Clientset: cs}, nil
}

// CrossCompileLinuxBinary builds a static Linux opensandbox-bench binary (CGO_ENABLED=0)
// for isomorphic injection into the in-cluster Bench Runner Pod (OSEP-0034 §1).
func CrossCompileLinuxBinary(ctx context.Context, moduleDir, goArch, outputPath string) error {
	if goArch == "" {
		goArch = "amd64"
	}
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		return fmt.Errorf("create output dir: %w", err)
	}
	cmd := exec.CommandContext(ctx, "go", "build", "-trimpath", "-o", outputPath, "./cmd/opensandbox-bench")
	cmd.Dir = moduleDir
	cmd.Env = append(os.Environ(),
		"CGO_ENABLED=0",
		"GOOS=linux",
		"GOARCH="+goArch,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("cross-compile linux/%s worker binary: %w\n%s", goArch, err, string(out))
	}
	return nil
}

// RemoteRunner abstracts the in-cluster Bench Bastion/Runner Pod lifecycle so all
// workload traffic and white-box scraping execute inside the cluster network.
type RemoteRunner interface {
	EnsureRunnerPod(ctx context.Context, prof *profile.BenchmarkProfile) (podName string, err error)
	DeleteRunnerPod(ctx context.Context, namespace, podName string) error
	DetectClusterArch(ctx context.Context, namespace, podName string) (string, error)
	InjectBinary(ctx context.Context, namespace, podName, localBinaryPath, remotePath string) error
	InjectBytes(ctx context.Context, namespace, podName string, data []byte, remotePath string, mode int64) error
	ExecWorker(ctx context.Context, namespace, podName, remoteBinaryPath string, args []string, stdout, stderr io.Writer) error
	PullBundle(ctx context.Context, namespace, podName, remoteBundleDir, localOutputDir string) error
}
