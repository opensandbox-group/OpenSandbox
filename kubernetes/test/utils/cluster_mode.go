// Copyright 2025 Alibaba Group Holding Ltd.
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

package utils

import "os"

const (
	// ModeKind runs e2e against a local Kind cluster (default).
	ModeKind = "kind"
	// ModeExternal runs e2e against an externally-provided Kubernetes cluster
	// using the kubeconfig pointed to by KUBECONFIG. Use this when targeting
	// minikube, a shared dev cluster, a CI-provisioned cluster, etc.
	ModeExternal = "external"
)

// Mode returns the e2e cluster mode. Reads E2E_MODE env, defaults to ModeKind.
func Mode() string {
	if v := os.Getenv("E2E_MODE"); v != "" {
		return v
	}
	return ModeKind
}

// IsKind reports whether the current e2e mode is the local Kind cluster.
func IsKind() bool { return Mode() == ModeKind }

// IsExternal reports whether the current e2e mode targets an
// externally-provided cluster via KUBECONFIG.
func IsExternal() bool { return !IsKind() }

// SkipImageBuild reports whether the suite should skip the docker-build /
// kind-load steps in BeforeSuite. Non-Kind modes default to true because
// images are expected to be pre-built and pushed to a registry the target
// cluster can pull from.
func SkipImageBuild() bool {
	if v := os.Getenv("SKIP_IMAGE_BUILD"); v != "" {
		return v == "1" || v == "true" || v == "TRUE"
	}
	return IsExternal()
}

// RegistrySourceImage returns the upstream registry image used by the
// in-cluster docker-registry deployment for pause/resume tests. Override
// via REGISTRY_SOURCE_IMAGE to point at a mirror reachable from the target
// cluster.
func RegistrySourceImage() string {
	if v := os.Getenv("REGISTRY_SOURCE_IMAGE"); v != "" {
		return v
	}
	return "registry:2"
}

// AlpineImage returns the alpine image used by commit jobs.
func AlpineImage() string {
	if v := os.Getenv("ALPINE_IMAGE"); v != "" {
		return v
	}
	return "alpine:latest"
}

// PauseResumeNamespace returns the namespace used by pause/resume tests for
// the in-cluster docker-registry and registry secrets.
func PauseResumeNamespace() string {
	if v := os.Getenv("PAUSE_RESUME_NAMESPACE"); v != "" {
		return v
	}
	return "default"
}

// PauseResumeRegistryAddr returns the in-cluster docker-registry service
// address used by pause/resume tests. When unset, it derives the host from
// PauseResumeNamespace() so overriding only PAUSE_RESUME_NAMESPACE is
// sufficient and credential auths stay aligned with the registry endpoint.
func PauseResumeRegistryAddr() string {
	if v := os.Getenv("PAUSE_RESUME_REGISTRY_ADDR"); v != "" {
		return v
	}
	return "docker-registry." + PauseResumeNamespace() + ".svc.cluster.local:5000"
}

// PauseResumeRegistryUser returns the registry username for pause/resume tests.
func PauseResumeRegistryUser() string {
	if v := os.Getenv("PAUSE_RESUME_REGISTRY_USER"); v != "" {
		return v
	}
	return "testuser"
}

// PauseResumeRegistryPass returns the registry password for pause/resume tests.
func PauseResumeRegistryPass() string {
	if v := os.Getenv("PAUSE_RESUME_REGISTRY_PASS"); v != "" {
		return v
	}
	return "testpass"
}

// PodSecurityEnforce returns the value applied to the
// `pod-security.kubernetes.io/enforce` namespace label. Empty string means
// the suite must skip applying the label (some platforms reject restricted).
func PodSecurityEnforce() string {
	if v, ok := os.LookupEnv("E2E_POD_SECURITY_ENFORCE"); ok {
		return v
	}
	return "restricted"
}
