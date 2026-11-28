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

package collector

import (
	"context"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	TelemetryStatusOK       = "ok"
	TelemetryStatusDegraded = "degraded"
)

// TelemetryPreflightResult summarizes whether the in-cluster worker has the
// required Kubernetes RBAC and runtime pod reachability for deterministic
// benchmark telemetry and post-run leak auditing.
type TelemetryPreflightResult struct {
	Status  string   `json:"status"`
	Missing []string `json:"missing,omitempty"`
}

// OK reports whether all required telemetry prerequisites passed.
func (r TelemetryPreflightResult) OK() bool {
	return r.Status == TelemetryStatusOK && len(r.Missing) == 0
}

// RunTelemetryPreflight verifies nodes:list, pods:list, sandbox.fast.io CR
// listing, firecracker-runtime pod reachability, and pods/exec:create before
// benchmark suites execute.
func RunTelemetryPreflight(ctx context.Context, cfg Config, clients *Clients) TelemetryPreflightResult {
	cfg.applyDefaults()
	res := TelemetryPreflightResult{Status: TelemetryStatusOK}
	if clients == nil || clients.Clientset == nil {
		res.Status = TelemetryStatusDegraded
		res.Missing = append(res.Missing, "kubernetes clientset unavailable")
		return res
	}

	addMissing := func(item string) {
		res.Status = TelemetryStatusDegraded
		res.Missing = append(res.Missing, item)
	}

	// 1. nodes:list (required for node inventory and high-density-soak memory sizing).
	if _, err := clients.Clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{Limit: 1}); err != nil {
		addMissing(fmt.Sprintf("nodes:list failed: %v", err))
	}

	// 2. pods:list across system and resource namespaces.
	for _, ns := range uniqueNonEmpty(cfg.SystemNamespace, cfg.ResourceNamespace) {
		if _, err := clients.Clientset.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{Limit: 1}); err != nil {
			addMissing(fmt.Sprintf("pods:list in namespace %q failed: %v", ns, err))
		}
	}

	// 3. sandbox.fast.io CR list access (when dynamic client is configured).
	if clients.DynamicClient != nil {
		var crErr error
		checked := false
		for _, gvr := range []struct {
			name string
			list func() error
		}{
			{
				name: "v1alpha2",
				list: func() error {
					_, err := clients.DynamicClient.Resource(SandboxGVRV1Alpha2).Namespace(cfg.ResourceNamespace).List(ctx, metav1.ListOptions{Limit: 1})
					return err
				},
			},
			{
				name: "v1alpha1",
				list: func() error {
					_, err := clients.DynamicClient.Resource(SandboxGVRV1Alpha1).Namespace(cfg.ResourceNamespace).List(ctx, metav1.ListOptions{Limit: 1})
					return err
				},
			},
		} {
			err := gvr.list()
			if err == nil {
				checked = true
				crErr = nil
				break
			}
			if apierrors.IsNotFound(err) {
				crErr = err
				continue
			}
			crErr = err
			break
		}
		if !checked && crErr != nil && !apierrors.IsNotFound(crErr) {
			addMissing(fmt.Sprintf("sandbox.fast.io/sandboxes:list in namespace %q failed: %v", cfg.ResourceNamespace, crErr))
		}
	}

	// 4. Runtime pod discovery and pods/exec:create verification (when execer is configured).
	if clients.Execer != nil {
		rtPods, err := listFirecrackerRuntimePods(ctx, clients.Clientset, cfg.SystemNamespace, cfg.ResourceNamespace)
		if err != nil {
			addMissing(fmt.Sprintf("firecracker-runtime pods:list failed: %v", err))
		} else if len(rtPods) == 0 {
			addMissing(fmt.Sprintf("no running firecracker-runtime pods found in %q or %q", cfg.SystemNamespace, cfg.ResourceNamespace))
		} else {
			pod := &rtPods[0]
			container := pickRuntimeContainer(pod, DefaultJanitorContainer, DefaultUblkdContainer, DefaultFirecrackerAgentContainer)
			_, stderr, execErr := execCommand(ctx, clients.Execer, pod.Namespace, pod.Name, container, []string{"/bin/sh", "-c", "true"}, cfg.ExecTimeout)
			if execErr != nil {
				detail := strings.TrimSpace(stderr)
				if detail != "" {
					addMissing(fmt.Sprintf("pods/exec:create on %s/%s (%s) failed: %v (%s)", pod.Namespace, pod.Name, container, execErr, detail))
				} else {
					addMissing(fmt.Sprintf("pods/exec:create on %s/%s (%s) failed: %v", pod.Namespace, pod.Name, container, execErr))
				}
			}
		}
	}

	return res
}
