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

package provisioner

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/profile"
	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/report"
)

const leakAuditContainerName = "audit"

// Up provisions the full Phase 1 fast-sandbox OverlayBD benchmark stack:
// namespaces + Runner RBAC, optional host preparation, ephemeral HTTP registry,
// conditional Redis, OpenSandbox + fast-sandbox Helm chart (tracing disabled),
// readiness checks, golden OverlayBD SandboxTemplate, and ResourceAware SandboxPool.
func (p *OverlayBDProvisioner) Up(ctx context.Context, prof *profile.BenchmarkProfile, opts Options) error {
	if prof == nil {
		return fmt.Errorf("benchmark profile must not be nil")
	}
	if err := prof.Validate(); err != nil {
		return fmt.Errorf("invalid benchmark profile: %w", err)
	}

	rc, err := p.resolveClients(opts)
	if err != nil {
		return err
	}

	systemNS, resourceNS, benchNS := resolveNamespaces(prof)

	// 1. Namespaces & Runner Pod RBAC.
	for _, ns := range uniqueStrings(systemNS, resourceNS, benchNS) {
		if err := ensureNamespace(ctx, rc.kube, ns); err != nil {
			return err
		}
	}
	if err := p.ensureRunnerRBAC(ctx, rc.kube, benchNS); err != nil {
		return err
	}
	if err := p.ensureFastSandboxLoggingConfig(ctx, rc.kube, systemNS, resourceNS); err != nil {
		return err
	}
	if err := p.ensureSecureAccessSecret(ctx, rc.kube, systemNS); err != nil {
		return err
	}

	// 2. Opt-in host node preparation.
	if opts.PrepareNodes || prof.Environment.PrepareNodes {
		if err := p.prepareNodes(ctx, rc, prof, benchNS); err != nil {
			return err
		}
	}

	// 3. Supporting infrastructure in benchNamespace and optional in-cluster OTel stack.
	if prof.Environment.Registry.EphemeralInCluster {
		if err := p.deployEphemeralRegistry(ctx, rc, prof, systemNS, resourceNS, benchNS); err != nil {
			return err
		}
	} else {
		if err := p.ensureAgentRegistrySecretIfMissing(ctx, rc.kube, systemNS); err != nil {
			return err
		}
	}
	if err := p.ensureFastSandboxRegistryConfig(ctx, rc.kube, prof, resourceNS, benchNS); err != nil {
		return err
	}
	if prof.Environment.Redis.Enabled {
		if err := p.deployRedis(ctx, rc, prof, benchNS); err != nil {
			return err
		}
	}
	if prof.Environment.Tracing.InCluster {
		if err := p.deployOTelStack(ctx, rc, prof); err != nil {
			return err
		}
	}

	// 4. OpenSandbox + fast-sandbox Helm deployment and readiness verification.
	if err := p.installHelmStack(ctx, rc, prof, opts, systemNS); err != nil {
		return err
	}
	if err := p.waitForStackReady(ctx, rc, prof, systemNS); err != nil {
		return err
	}

	// 5. Golden OverlayBD SandboxTemplate and ResourceAware SandboxPool.
	if err := p.ensureGoldenTemplate(ctx, rc, prof, resourceNS, benchNS); err != nil {
		return err
	}
	if err := p.ensureResourceAwarePool(ctx, rc, prof, resourceNS); err != nil {
		return err
	}

	return nil
}

func (p *OverlayBDProvisioner) ensureGoldenTemplate(ctx context.Context, rc *resolvedClients, prof *profile.BenchmarkProfile, resourceNS, benchNS string) error {
	if rc.dyn == nil {
		return fmt.Errorf("dynamic client is required to manage SandboxTemplate CRs")
	}

	tmpl := buildGoldenTemplateCR(prof, resourceNS, benchNS)
	res := rc.dyn.Resource(sandboxTemplateGVR).Namespace(resourceNS)
	if err := upsertUnstructured(ctx, res, tmpl); err != nil {
		return fmt.Errorf("apply SandboxTemplate %s/%s: %w", resourceNS, defaultTemplateName, err)
	}

	waitCtx, cancel := context.WithTimeout(ctx, p.readyTimeout())
	defer cancel()
	ticker := time.NewTicker(p.pollInterval())
	defer ticker.Stop()

	for {
		obj, err := res.Get(waitCtx, defaultTemplateName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("poll SandboxTemplate %s/%s: %w", resourceNS, defaultTemplateName, err)
		}
		phase, _, _ := unstructured.NestedString(obj.Object, "status", "phase")
		manifestRef, _, _ := unstructured.NestedString(obj.Object, "status", "manifestRef")
		switch phase {
		case "Succeeded":
			if !strings.HasPrefix(manifestRef, "oci://") {
				return fmt.Errorf("SandboxTemplate %s/%s succeeded with non-OCI manifestRef %q (expected oci:// prefix)", resourceNS, defaultTemplateName, manifestRef)
			}
			rc.logger.Info("golden OverlayBD SandboxTemplate ready", "template", defaultTemplateName, "manifestRef", manifestRef)
			return nil
		case "Failed":
			return fmt.Errorf("SandboxTemplate %s/%s build failed", resourceNS, defaultTemplateName)
		}

		select {
		case <-waitCtx.Done():
			return fmt.Errorf("timed out waiting for SandboxTemplate %s/%s to succeed: %w", resourceNS, defaultTemplateName, waitCtx.Err())
		case <-ticker.C:
		}
	}
}

func buildGoldenTemplateCR(prof *profile.BenchmarkProfile, resourceNS, benchNS string) *unstructured.Unstructured {
	tpl := prof.Stack.Template
	vcpus := tpl.VCPUs
	if vcpus <= 0 {
		vcpus = 1
	}
	memMiB := tpl.MemoryMiB
	if memMiB <= 0 {
		memMiB = 2048
	}
	diskGiB := tpl.DiskGiB
	if diskGiB <= 0 {
		diskGiB = prof.Stack.FastSandbox.Runtime.OCI.PoolSizeGB
	}

	publishTarget := strings.TrimSpace(tpl.Publish)
	if publishTarget == "" {
		publishTarget = fmt.Sprintf("oci://%s/opensandbox/templates", ephemeralRegistryHost(benchNS))
	}
	publishSecret := strings.TrimSpace(tpl.PublishSecretRef)
	if publishSecret == "" {
		publishSecret = ociPublishSecretName
	}

	spec := map[string]any{
		"image":  tpl.BaseImage,
		"kernel": "vmlinux.bin",
		"machine": map[string]any{
			"vcpu":   strconv.Itoa(vcpus),
			"memory": fmt.Sprintf("%dMi", memMiB),
		},
		"output": map[string]any{
			"format":     "overlaybd",
			"rootfsSize": fmt.Sprintf("%dGi", diskGiB),
			"publish":    publishTarget,
			"publishSecretRef": map[string]any{
				"name": publishSecret,
			},
		},
		"readiness": map[string]any{
			"warmupSeconds": int64(60),
		},
	}
	execdImg := strings.TrimSpace(tpl.ExecdImage)
	if execdImg == "" {
		execdImg = defaultExecdImage
	}
	spec["execd"] = applyImageMirror(prof, execdImg)

	return &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "sandbox.fast.io/v1alpha2",
			"kind":       "SandboxTemplate",
			"metadata": map[string]any{
				"name":      defaultTemplateName,
				"namespace": resourceNS,
				"labels": map[string]any{
					managedByLabelKey: managedByLabelValue,
				},
			},
			"spec": spec,
		},
	}
}

func (p *OverlayBDProvisioner) ensureResourceAwarePool(ctx context.Context, rc *resolvedClients, prof *profile.BenchmarkProfile, resourceNS string) error {
	if rc.dyn == nil {
		return fmt.Errorf("dynamic client is required to manage SandboxPool CRs")
	}

	poolCR := buildResourceAwarePoolCR(prof, resourceNS)
	res := rc.dyn.Resource(sandboxPoolGVR).Namespace(resourceNS)
	if err := upsertUnstructured(ctx, res, poolCR); err != nil {
		return fmt.Errorf("apply SandboxPool %s/%s: %w", resourceNS, defaultPoolName, err)
	}

	minFastlets := prof.Stack.FastSandbox.Pool.MinFastlets
	egressEnabled := prof.Stack.FastSandbox.Pool.EgressEnabled

	waitCtx, cancel := context.WithTimeout(ctx, p.readyTimeout())
	defer cancel()
	ticker := time.NewTicker(p.pollInterval())
	defer ticker.Stop()

	for {
		pods, err := rc.kube.CoreV1().Pods(resourceNS).List(waitCtx, metav1.ListOptions{
			LabelSelector: "app=sandbox-fastlet",
		})
		if err != nil {
			return fmt.Errorf("list fastlet pods in %q: %w", resourceNS, err)
		}
		readyFastlets := 0
		for _, pod := range pods.Items {
			if pod.Status.Phase != corev1.PodRunning || !isPodReady(&pod) {
				continue
			}
			if egressEnabled && !isContainerReady(&pod, "egress") {
				continue
			}
			readyFastlets++
		}
		if readyFastlets >= minFastlets {
			rc.logger.Info("ResourceAware SandboxPool ready", "pool", defaultPoolName, "readyFastlets", readyFastlets, "egressEnabled", egressEnabled)
			return nil
		}

		select {
		case <-waitCtx.Done():
			return fmt.Errorf("timed out waiting for %d Ready fastlet pods (egress=%v) in %q: %w", minFastlets, egressEnabled, resourceNS, waitCtx.Err())
		case <-ticker.C:
		}
	}
}

func buildResourceAwarePoolCR(prof *profile.BenchmarkProfile, resourceNS string) *unstructured.Unstructured {
	pool := prof.Stack.FastSandbox.Pool
	tpl := prof.Stack.Template

	vcpus := tpl.VCPUs
	if vcpus <= 0 {
		vcpus = 1
	}
	memMiB := tpl.MemoryMiB
	if memMiB <= 0 {
		memMiB = 2048
	}
	cpuRatio := pool.OvercommitRatio.CPU
	if cpuRatio < 1 {
		cpuRatio = 1
	}
	memRatio := pool.OvercommitRatio.Memory
	if memRatio < 1 {
		memRatio = 1
	}

	fastletEnv := []any{
		map[string]any{"name": "FAST_SANDBOX_RUNTIME_AGENT_SOCKET", "value": "/run/fast-sandbox/firecracker/runtime.sock"},
		map[string]any{"name": "FAST_SANDBOX_OCI_ENABLED", "value": "true"},
	}
	if strings.TrimSpace(pool.SpillDir) != "" {
		fastletEnv = append(fastletEnv, map[string]any{
			"name":  "FAST_SANDBOX_SNAPSHOT_SPILL_DIR",
			"value": pool.SpillDir,
		})
	}
	if pool.BlobCacheMaxBytes > 0 {
		fastletEnv = append(fastletEnv, map[string]any{
			"name":  "FAST_SANDBOX_OCI_BLOB_CACHE_SIZE_BYTES",
			"value": strconv.FormatInt(pool.BlobCacheMaxBytes, 10),
		})
	}

	fastletMounts := []any{
		map[string]any{"name": "agent-socket", "mountPath": "/run/fast-sandbox/firecracker"},
		map[string]any{"name": "oci-tools", "mountPath": "/opt/fast-sandbox/oci", "readOnly": true},
		map[string]any{"name": "oci-socket", "mountPath": "/run/fast-sandbox/overlaybd"},
		map[string]any{"name": "host-dev", "mountPath": "/dev"},
	}

	fastletImage := strings.TrimSpace(pool.FastletImage)
	if fastletImage == "" {
		fastletImage = defaultFSBFastletImage
	}
	fastletImage = applyImageMirror(prof, fastletImage)

	containers := []any{
		map[string]any{
			"name":            "fastlet",
			"image":           fastletImage,
			"imagePullPolicy": "IfNotPresent",
			"env":             fastletEnv,
			"volumeMounts":    fastletMounts,
		},
	}

	volumes := []any{
		map[string]any{
			"name": "agent-socket",
			"hostPath": map[string]any{
				"path": "/run/fast-sandbox/firecracker",
				"type": "DirectoryOrCreate",
			},
		},
		map[string]any{
			"name": "oci-tools",
			"hostPath": map[string]any{
				"path": "/opt/fast-sandbox/oci",
				"type": "DirectoryOrCreate",
			},
		},
		map[string]any{
			"name": "oci-socket",
			"hostPath": map[string]any{
				"path": "/run/fast-sandbox/overlaybd",
				"type": "DirectoryOrCreate",
			},
		},
		map[string]any{
			"name": "host-dev",
			"hostPath": map[string]any{
				"path": "/dev",
				"type": "Directory",
			},
		},
	}

	poolMode := strings.TrimSpace(pool.Mode)
	if poolMode == "" {
		poolMode = profile.PoolModeResourceAware
	}
	sandboxResources := map[string]any{
		"mode":   poolMode,
		"cpu":    strconv.Itoa(vcpus),
		"memory": fmt.Sprintf("%dMi", memMiB),
		"pids":   int64(256),
	}
	if poolMode == profile.PoolModeResourceAware {
		sandboxResources["overcommitRatio"] = map[string]any{
			"cpu":    strconv.FormatFloat(cpuRatio, 'f', -1, 64),
			"memory": strconv.FormatFloat(memRatio, 'f', -1, 64),
		}
	}

	maxSandboxesPerPod := pool.MaxSandboxesPerPod
	if maxSandboxesPerPod <= 0 {
		maxSandboxesPerPod = 500
	}

	spec := map[string]any{
		"runtime":            "firecracker",
		"maxSandboxesPerPod": int64(maxSandboxesPerPod),
		"capacity": map[string]any{
			"poolMin":   int64(pool.MinFastlets),
			"poolMax":   int64(pool.MaxFastlets),
			"bufferMin": int64(0),
			"bufferMax": int64(0),
		},
		"sandboxResources": sandboxResources,
	}

	if pool.EgressEnabled {
		egressImage := strings.TrimSpace(pool.EgressImage)
		if egressImage == "" {
			egressImage = defaultEgressImage
		}
		egressImage = applyImageMirror(prof, egressImage)
		egressEnv := []any{
			map[string]any{"name": "OPENSANDBOX_EGRESS_PROFILE", "value": "fast-sandbox"},
			map[string]any{"name": "OPENSANDBOX_EGRESS_MODE", "value": "dns+nft"},
			map[string]any{"name": "OPENSANDBOX_EGRESS_VAULT_DIR", "value": "/var/lib/opensandbox-egress/vault"},
			map[string]any{"name": "OPENSANDBOX_EGRESS_POLICY_DIR", "value": "/var/lib/opensandbox-egress/policy"},
		}
		if prof.Environment.Tracing.InCluster {
			egressEnv = append(egressEnv, otelMetricsEnvMaps()...)
		}
		containers = append(containers, map[string]any{
			"name":            "egress",
			"image":           egressImage,
			"imagePullPolicy": "IfNotPresent",
			"env":             egressEnv,
			"volumeMounts": []any{
				map[string]any{"name": "egress-vault", "mountPath": "/var/lib/opensandbox-egress"},
				map[string]any{"name": "firecracker-state", "mountPath": defaultStateRoot},
			},
			"securityContext": map[string]any{
				"capabilities": map[string]any{
					"add": []any{"NET_ADMIN", "NET_RAW"},
				},
			},
			"resources": buildContainerResources(pool.EgressResources, "2", "4Gi", "4", "16Gi"),
		})
		volumes = append(volumes, map[string]any{
			"name":     "egress-vault",
			"emptyDir": map[string]any{},
		})
		spec["infraComponents"] = []any{
			map[string]any{
				"name":     "egress",
				"delivery": "host-process",
				"process": map[string]any{
					"command": []any{"/bin/egress"},
					"healthCheck": map[string]any{
						"tcpConnect":     map[string]any{},
						"timeoutSeconds": int64(10),
					},
				},
				"endpoint": map[string]any{
					"protocol": "HTTP",
					"port":     int64(18080),
				},
			},
		}
		spec["actionHandlers"] = []any{
			map[string]any{
				"name":           "egress",
				"targetHTTPPort": int64(18080),
				"hooks":          []any{"sandbox.runtime-ready", "sandbox.data-plane-ready"},
			},
		}
	}

	fastletTemplateSpec := map[string]any{
		"containers": containers,
		"volumes":    volumes,
		"nodeSelector": map[string]any{
			firecrackerNodeLabelKey: "true",
		},
	}
	if pool.PodAntiAffinity == nil || *pool.PodAntiAffinity {
		fastletTemplateSpec["affinity"] = map[string]any{
			"podAntiAffinity": map[string]any{
				"requiredDuringSchedulingIgnoredDuringExecution": []any{
					map[string]any{
						"labelSelector": map[string]any{
							"matchLabels": map[string]any{"app": "sandbox-fastlet"},
						},
						"topologyKey": "kubernetes.io/hostname",
					},
				},
			},
		}
	}
	if len(pool.ImagePullSecrets) > 0 {
		pullSecrets := make([]any, 0, len(pool.ImagePullSecrets))
		for _, s := range pool.ImagePullSecrets {
			if name := strings.TrimSpace(s); name != "" {
				pullSecrets = append(pullSecrets, map[string]any{"name": name})
			}
		}
		if len(pullSecrets) > 0 {
			fastletTemplateSpec["imagePullSecrets"] = pullSecrets
		}
	}

	spec["fastletTemplate"] = map[string]any{
		"spec": fastletTemplateSpec,
	}

	return &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "sandbox.fast.io/v1alpha2",
			"kind":       "SandboxPool",
			"metadata": map[string]any{
				"name":      defaultPoolName,
				"namespace": resourceNS,
				"labels": map[string]any{
					managedByLabelKey: managedByLabelValue,
				},
			},
			"spec": spec,
		},
	}
}

func buildContainerResources(res *profile.ContainerResourcesSpec, defReqCPU, defReqMem, defLimCPU, defLimMem string) map[string]any {
	reqCPU, reqMem, limCPU, limMem := defReqCPU, defReqMem, defLimCPU, defLimMem
	if res != nil {
		if v := strings.TrimSpace(res.Requests.CPU); v != "" {
			reqCPU = v
		}
		if v := strings.TrimSpace(res.Requests.Memory); v != "" {
			reqMem = v
		}
		if v := strings.TrimSpace(res.Limits.CPU); v != "" {
			limCPU = v
		}
		if v := strings.TrimSpace(res.Limits.Memory); v != "" {
			limMem = v
		}
	}
	return map[string]any{
		"requests": map[string]any{"cpu": reqCPU, "memory": reqMem},
		"limits":   map[string]any{"cpu": limCPU, "memory": limMem},
	}
}

type unstructuredClient interface {
	Get(ctx context.Context, name string, opts metav1.GetOptions, subresources ...string) (*unstructured.Unstructured, error)
	Create(ctx context.Context, obj *unstructured.Unstructured, opts metav1.CreateOptions, subresources ...string) (*unstructured.Unstructured, error)
	Update(ctx context.Context, obj *unstructured.Unstructured, opts metav1.UpdateOptions, subresources ...string) (*unstructured.Unstructured, error)
}

func upsertUnstructured(ctx context.Context, res unstructuredClient, obj *unstructured.Unstructured) error {
	existing, err := res.Get(ctx, obj.GetName(), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = res.Create(ctx, obj, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	obj.SetResourceVersion(existing.GetResourceVersion())
	_, err = res.Update(ctx, obj, metav1.UpdateOptions{})
	return err
}

// Down deletes benchmark CRs (Sandbox, SandboxSnapshot, SandboxPool, SandboxTemplate),
// uninstalls the Helm release, removes ephemeral registry/Redis/bench resources,
// and runs a post-teardown leak audit returning *report.LeakAudit.
func (p *OverlayBDProvisioner) Down(ctx context.Context, prof *profile.BenchmarkProfile, opts Options) (*report.LeakAudit, error) {
	if prof == nil {
		return nil, fmt.Errorf("benchmark profile must not be nil")
	}

	rc, err := p.resolveClients(opts)
	if err != nil {
		return nil, err
	}

	systemNS, resourceNS, benchNS := resolveNamespaces(prof)

	// 1. Delete benchmark CRs in reverse dependency order.
	if rc.dyn != nil {
		for _, gvr := range []schema.GroupVersionResource{sandboxGVR, sandboxSnapshotGVR, sandboxPoolGVR, sandboxTemplateGVR} {
			if err := deleteAllCRs(ctx, rc, gvr, resourceNS); err != nil {
				rc.logger.Warn("failed deleting CRs during teardown", "resource", gvr.Resource, "namespace", resourceNS, "error", err)
			}
		}
	}

	// 2. Uninstall Helm release.
	if err := p.uninstallHelmStack(ctx, rc, opts, systemNS); err != nil {
		return nil, err
	}

	// 3. Delete ephemeral registry, Redis, Runner RBAC, and bench secrets/PVCs.
	p.deleteBenchResources(ctx, rc, systemNS, resourceNS, benchNS)

	// 4. Run post-teardown leak audit.
	audit, err := p.runPostTeardownLeakAudit(ctx, rc, prof, resourceNS, benchNS)
	if err != nil {
		return audit, err
	}
	return audit, nil
}

func deleteAllCRs(ctx context.Context, rc *resolvedClients, gvr schema.GroupVersionResource, namespace string) error {
	res := rc.dyn.Resource(gvr).Namespace(namespace)
	list, err := res.List(ctx, metav1.ListOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	for _, item := range list.Items {
		if err := res.Delete(ctx, item.GetName(), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete %s %s/%s: %w", gvr.Resource, namespace, item.GetName(), err)
		}
	}
	return nil
}

func (p *OverlayBDProvisioner) deleteBenchResources(ctx context.Context, rc *resolvedClients, systemNS, resourceNS, benchNS string) {
	ignoreNotFound := func(err error, what string) {
		if err != nil && !apierrors.IsNotFound(err) {
			rc.logger.Warn("teardown resource deletion warning", "resource", what, "error", err)
		}
	}

	for _, ns := range uniqueStrings(systemNS, resourceNS) {
		ignoreNotFound(rc.kube.CoreV1().ConfigMaps(ns).Delete(ctx, fastSandboxLoggingConfigMapName, metav1.DeleteOptions{}), ns+"/configmap/"+fastSandboxLoggingConfigMapName)
	}
	ignoreNotFound(rc.kube.CoreV1().ConfigMaps(resourceNS).Delete(ctx, fastSandboxRegistryConfigMapName, metav1.DeleteOptions{}), resourceNS+"/configmap/"+fastSandboxRegistryConfigMapName)
	ignoreNotFound(rc.kube.CoreV1().Secrets(systemNS).Delete(ctx, secureAccessSecretName, metav1.DeleteOptions{}), systemNS+"/secret/"+secureAccessSecretName)

	// Ephemeral registry + Redis in benchNS.
	ignoreNotFound(rc.kube.AppsV1().Deployments(benchNS).Delete(ctx, ephemeralRegistryName, metav1.DeleteOptions{}), "deployment/"+ephemeralRegistryName)
	ignoreNotFound(rc.kube.CoreV1().Services(benchNS).Delete(ctx, ephemeralRegistryName, metav1.DeleteOptions{}), "service/"+ephemeralRegistryName)
	ignoreNotFound(rc.kube.CoreV1().PersistentVolumeClaims(benchNS).Delete(ctx, ephemeralRegistryName+"-data", metav1.DeleteOptions{}), "pvc/"+ephemeralRegistryName+"-data")
	ignoreNotFound(rc.kube.CoreV1().Secrets(benchNS).Delete(ctx, ephemeralRegistryTLSSecret, metav1.DeleteOptions{}), "secret/"+ephemeralRegistryTLSSecret)

	ignoreNotFound(rc.kube.AppsV1().Deployments(benchNS).Delete(ctx, ephemeralRedisName, metav1.DeleteOptions{}), "deployment/"+ephemeralRedisName)
	ignoreNotFound(rc.kube.CoreV1().Services(benchNS).Delete(ctx, ephemeralRedisName, metav1.DeleteOptions{}), "service/"+ephemeralRedisName)

	// Ephemeral OTel Collector + Jaeger stack in opensandbox-bench-obs.
	ignoreNotFound(rc.kube.AppsV1().Deployments(otelObsNamespace).Delete(ctx, otelCollectorName, metav1.DeleteOptions{}), otelObsNamespace+"/deployment/"+otelCollectorName)
	ignoreNotFound(rc.kube.CoreV1().Services(otelObsNamespace).Delete(ctx, otelCollectorName, metav1.DeleteOptions{}), otelObsNamespace+"/service/"+otelCollectorName)
	ignoreNotFound(rc.kube.CoreV1().ConfigMaps(otelObsNamespace).Delete(ctx, otelCollectorConfigMapName, metav1.DeleteOptions{}), otelObsNamespace+"/configmap/"+otelCollectorConfigMapName)
	ignoreNotFound(rc.kube.AppsV1().Deployments(otelObsNamespace).Delete(ctx, otelJaegerName, metav1.DeleteOptions{}), otelObsNamespace+"/deployment/"+otelJaegerName)
	ignoreNotFound(rc.kube.CoreV1().Services(otelObsNamespace).Delete(ctx, otelJaegerName, metav1.DeleteOptions{}), otelObsNamespace+"/service/"+otelJaegerName)
	ignoreNotFound(rc.kube.CoreV1().Namespaces().Delete(ctx, otelObsNamespace, metav1.DeleteOptions{}), "namespace/"+otelObsNamespace)

	// Distributed CA and registry secrets.
	for _, ns := range uniqueStrings(benchNS, systemNS, resourceNS) {
		ignoreNotFound(rc.kube.CoreV1().Secrets(ns).Delete(ctx, ephemeralRegistryCASecret, metav1.DeleteOptions{}), "secret/"+ns+"/"+ephemeralRegistryCASecret)
	}
	ignoreNotFound(rc.kube.CoreV1().Secrets(systemNS).Delete(ctx, agentRegistrySecretName, metav1.DeleteOptions{}), "secret/"+systemNS+"/"+agentRegistrySecretName)
	for _, ns := range uniqueStrings(resourceNS, systemNS) {
		ignoreNotFound(rc.kube.CoreV1().Secrets(ns).Delete(ctx, ociPublishSecretName, metav1.DeleteOptions{}), "secret/"+ns+"/"+ociPublishSecretName)
	}

	// Runner RBAC & ServiceAccount.
	ignoreNotFound(rc.kube.RbacV1().ClusterRoleBindings().Delete(ctx, runnerClusterRoleName, metav1.DeleteOptions{}), "clusterrolebinding/"+runnerClusterRoleName)
	ignoreNotFound(rc.kube.RbacV1().ClusterRoles().Delete(ctx, runnerClusterRoleName, metav1.DeleteOptions{}), "clusterrole/"+runnerClusterRoleName)
	ignoreNotFound(rc.kube.CoreV1().ServiceAccounts(benchNS).Delete(ctx, runnerServiceAccountName, metav1.DeleteOptions{}), "serviceaccount/"+runnerServiceAccountName)
}

func (p *OverlayBDProvisioner) runPostTeardownLeakAudit(ctx context.Context, rc *resolvedClients, prof *profile.BenchmarkProfile, resourceNS, benchNS string) (*report.LeakAudit, error) {
	audit := &report.LeakAudit{Passed: true}

	if rc.dyn != nil {
		if list, err := rc.dyn.Resource(sandboxGVR).Namespace(resourceNS).List(ctx, metav1.ListOptions{}); err == nil {
			audit.ResidualSandboxCRs = len(list.Items)
		}
		if list, err := rc.dyn.Resource(sandboxSnapshotGVR).Namespace(resourceNS).List(ctx, metav1.ListOptions{}); err == nil {
			audit.ResidualSnapshotCRs = len(list.Items)
		}
	}

	if err := ensureNamespace(ctx, rc.kube, benchNS); err == nil {
		nodeList, err := rc.kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		if err == nil {
			probeImage := resolveProbeImage(prof)
			minHeadroom := -1.0
			for _, node := range selectCandidateNodes(nodeList.Items) {
				nodeRes, err := p.runNodeLeakAuditProbe(ctx, rc, benchNS, node.Name, probeImage, defaultStateRoot)
				if err != nil {
					audit.Failures = append(audit.Failures, fmt.Sprintf("node %q leak audit probe failed: %v", node.Name, err))
					continue
				}
				audit.OrphanedUblkDevices += nodeRes.orphanedUblk
				audit.LeakedFirecrackerPIDs += nodeRes.leakedFirecrackerPIDs
				audit.HostOOMKills += nodeRes.hostOOMKills
				if minHeadroom < 0 || nodeRes.stateRootFreeGiB < minHeadroom {
					minHeadroom = nodeRes.stateRootFreeGiB
				}
			}
			if minHeadroom >= 0 {
				audit.StateRootFreeHeadroomG = minHeadroom
			}
		}
	}

	if audit.ResidualSandboxCRs > 0 {
		audit.Failures = append(audit.Failures, fmt.Sprintf("%d residual Sandbox CR(s) remain in %s", audit.ResidualSandboxCRs, resourceNS))
	}
	if audit.ResidualSnapshotCRs > 0 {
		audit.Failures = append(audit.Failures, fmt.Sprintf("%d residual SandboxSnapshot CR(s) remain in %s", audit.ResidualSnapshotCRs, resourceNS))
	}
	if audit.OrphanedUblkDevices > 0 {
		audit.Failures = append(audit.Failures, fmt.Sprintf("%d orphaned ublk device(s) detected", audit.OrphanedUblkDevices))
	}
	if audit.LeakedFirecrackerPIDs > 0 {
		audit.Failures = append(audit.Failures, fmt.Sprintf("%d leaked firecracker PID(s) detected", audit.LeakedFirecrackerPIDs))
	}
	if audit.HostOOMKills > 0 {
		audit.Failures = append(audit.Failures, fmt.Sprintf("%d host OOM kill(s) detected", audit.HostOOMKills))
	}

	audit.Passed = len(audit.Failures) == 0
	return audit, nil
}

type nodeLeakAuditResult struct {
	orphanedUblk          int
	leakedFirecrackerPIDs int
	hostOOMKills          int
	stateRootFreeGiB      float64
}

func (p *OverlayBDProvisioner) runNodeLeakAuditProbe(
	ctx context.Context,
	rc *resolvedClients,
	namespace, nodeName, probeImage, stateRoot string,
) (*nodeLeakAuditResult, error) {
	podName := fmt.Sprintf("osb-leakaudit-%s", sanitizeDNSLabel(nodeName, 38))
	pod := buildLeakAuditPod(namespace, podName, nodeName, probeImage, stateRoot)

	_ = rc.kube.CoreV1().Pods(namespace).Delete(ctx, podName, metav1.DeleteOptions{GracePeriodSeconds: int64Ptr(0)})
	if _, err := rc.kube.CoreV1().Pods(namespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		return nil, fmt.Errorf("create leak audit pod %s/%s: %w", namespace, podName, err)
	}
	defer func() {
		cleanCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = rc.kube.CoreV1().Pods(namespace).Delete(cleanCtx, podName, metav1.DeleteOptions{GracePeriodSeconds: int64Ptr(0)})
	}()

	if err := p.waitForPodTerminal(ctx, rc.kube, namespace, podName); err != nil {
		return nil, err
	}
	logs, err := rc.logs.ReadLogs(ctx, rc.kube, namespace, podName, leakAuditContainerName)
	if err != nil {
		return nil, err
	}
	return parseLeakAuditOutput(logs), nil
}

func buildLeakAuditPod(namespace, podName, nodeName, image, stateRoot string) *corev1.Pod {
	script := `set -u
UBLK_COUNT=$(ls -1 /host-dev/ublkc* 2>/dev/null | wc -l | tr -d ' ')
FC_PIDS=$(pgrep -x firecracker 2>/dev/null | wc -l | tr -d ' ')
OOM_KILLS=$(awk '/^oom_kill /{print $2; exit}' /host-proc/vmstat 2>/dev/null || echo 0)
FREE_KB=$(df -kP /host-stateroot 2>/dev/null | awk 'NR==2{print $4}')
echo "ORPHANED_UBLK=${UBLK_COUNT:-0}"
echo "LEAKED_FC_PIDS=${FC_PIDS:-0}"
echo "HOST_OOM_KILLS=${OOM_KILLS:-0}"
echo "STATEROOT_FREE_KB=${FREE_KB:-0}"
`
	dirType := corev1.HostPathDirectory
	dirOrCreate := corev1.HostPathDirectoryOrCreate

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name": "opensandbox-bench-leak-audit",
				managedByLabelKey:        managedByLabelValue,
			},
		},
		Spec: corev1.PodSpec{
			NodeName:      nodeName,
			RestartPolicy: corev1.RestartPolicyNever,
			HostPID:       true,
			Tolerations: []corev1.Toleration{
				{Operator: corev1.TolerationOpExists},
			},
			Containers: []corev1.Container{
				{
					Name:    leakAuditContainerName,
					Image:   image,
					Command: []string{"sh", "-c", script},
					SecurityContext: &corev1.SecurityContext{
						Privileged: boolPtr(true),
					},
					VolumeMounts: []corev1.VolumeMount{
						{Name: "host-dev", MountPath: "/host-dev", ReadOnly: true},
						{Name: "host-proc", MountPath: "/host-proc", ReadOnly: true},
						{Name: "host-stateroot", MountPath: "/host-stateroot", ReadOnly: true},
					},
				},
			},
			Volumes: []corev1.Volume{
				{Name: "host-dev", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/dev", Type: &dirType}}},
				{Name: "host-proc", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/proc", Type: &dirType}}},
				{Name: "host-stateroot", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: stateRoot, Type: &dirOrCreate}}},
			},
		},
	}
}

func parseLeakAuditOutput(raw string) *nodeLeakAuditResult {
	res := &nodeLeakAuditResult{}
	for _, line := range strings.Split(raw, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch strings.TrimSpace(k) {
		case "ORPHANED_UBLK":
			res.orphanedUblk, _ = strconv.Atoi(v)
		case "LEAKED_FC_PIDS":
			res.leakedFirecrackerPIDs, _ = strconv.Atoi(v)
		case "HOST_OOM_KILLS":
			res.hostOOMKills, _ = strconv.Atoi(v)
		case "STATEROOT_FREE_KB":
			if kb, err := strconv.ParseFloat(v, 64); err == nil {
				res.stateRootFreeGiB = kb / (1024 * 1024)
			}
		}
	}
	return res
}
