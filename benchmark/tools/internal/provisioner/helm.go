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
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/profile"
)

// BuildHelmValues constructs the profile-pinned Helm values map matching the
// exact value keys of manifests/charts/opensandbox and its subcharts
// (base, fast-sandbox, opensandbox-server, ingress-gateway).
// Tracing is explicitly kept disabled in Phase 1 unless overridden via prof.Stack.HelmValues.
func BuildHelmValues(prof *profile.BenchmarkProfile) map[string]any {
	systemNS, resourceNS, benchNS := resolveNamespaces(prof)
	oci := prof.Stack.FastSandbox.Runtime.OCI
	fastpathEndpoint := fmt.Sprintf("fast-sandbox-fastpath.%s.svc.cluster.local:9090", systemNS)
	gatewayHost := fmt.Sprintf("opensandbox-ingress-gateway.%s.svc.cluster.local", systemNS)
	redisDSN := fmt.Sprintf("redis://%s.%s.svc.cluster.local:%d/0", ephemeralRedisName, benchNS, ephemeralRedisPort)

	execdImage := strings.TrimSpace(prof.Stack.Template.ExecdImage)
	if execdImage == "" {
		execdImage = applyImageMirror(prof, "sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/execd:release-1.3.0-rc.3")
	}
	egressImage := strings.TrimSpace(prof.Stack.FastSandbox.Pool.EgressImage)
	if egressImage == "" {
		egressImage = applyImageMirror(prof, "sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/egress:release-1.3.0-rc.3")
	}
	publishSecret := strings.TrimSpace(prof.Stack.Template.PublishSecretRef)
	if publishSecret == "" {
		publishSecret = ociPublishSecretName
	}

	configToml := buildServerConfigToml(resourceNS, fastpathEndpoint, defaultPoolName, execdImage, egressImage, publishSecret, prof.Environment.Redis.Enabled, redisDSN)

	gatewayValues := map[string]any{
		"dataplaneNamespace":   resourceNS,
		"providerType":         "fast-sandbox",
		"gatewayRouteMode":     "header",
		"fastpathEndpoint":     fastpathEndpoint,
		"fastSandboxNamespace": resourceNS,
		"activity": map[string]any{
			"enabled":  prof.Environment.Redis.Enabled,
			"redisDsn": redisDSN,
		},
		"wake": map[string]any{
			"enabled": prof.Environment.Redis.Enabled,
		},
		"secureAccess": map[string]any{
			"existingSecret": secureAccessSecretName,
		},
	}

	ociValues := map[string]any{
		"enabled":     oci.Enabled,
		"cacheSizeGB": oci.CacheSizeGB,
		"poolLow":     oci.PoolLow,
		"poolHigh":    oci.PoolHigh,
		"poolSizeGB":  oci.PoolSizeGB,
	}

	runtimeValues := map[string]any{
		"enabled":        true,
		"registrySecret": agentRegistrySecretName,
		"stateRoot":      defaultStateRoot,
		"oci":            ociValues,
	}

	controllerValues := map[string]any{
		"fastletProxy": map[string]any{
			"resources": buildContainerResources(prof.Stack.FastSandbox.Pool.FastletProxyResources, "2", "4Gi", "2", "4Gi"),
		},
	}

	fastSandboxValues := map[string]any{
		"enabled":           true,
		"systemNamespace":   systemNS,
		"resourceNamespace": resourceNS,
		"controller":        controllerValues,
		"runtime":           runtimeValues,
	}

	values := map[string]any{
		"base": map[string]any{
			"enabled": true,
			"fastSandbox": map[string]any{
				"namespaces": map[string]any{
					"createSystem": false,
					"create":       false,
					"system":       systemNS,
					"resources":    resourceNS,
				},
			},
		},
		"tracing": map[string]any{
			"enabled": false,
		},
		"fast-sandbox": fastSandboxValues,
		"ingress-gateway": map[string]any{
			"enabled": true,
			"gateway": gatewayValues,
		},
		"opensandbox-server": map[string]any{
			"server": map[string]any{
				"replicaCount": 1,
				"env": []any{
					map[string]any{
						"name":  "OPENSANDBOX_INSECURE_SERVER",
						"value": "YES",
					},
				},
				"gateway": map[string]any{
					"enabled":          true,
					"host":             gatewayHost,
					"gatewayRouteMode": "header",
					"secureAccess": map[string]any{
						"existingSecret": secureAccessSecretName,
					},
				},
			},
			"configToml": configToml,
		},
	}

	if len(prof.Stack.HelmValues) > 0 {
		mergeMaps(values, prof.Stack.HelmValues)
	}
	return values
}

func buildServerConfigToml(resourceNS, fastpathEndpoint, poolName, execdImage, egressImage, publishSecret string, redisEnabled bool, redisDSN string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `[server]
host = "0.0.0.0"
port = 80
api_key = ""

[log]
level = "INFO"

[runtime]
type = "kubernetes"
execd_image = %q

[kubernetes]
kubeconfig_path = ""
namespace = %q
workload_provider = "batchsandbox"
fastpath_endpoint = %q
fastpath_resource_pool = %q
template_oci_publish_secret = %q

[egress]
image = %q
mode = "dns+nft"
`, execdImage, resourceNS, fastpathEndpoint, poolName, publishSecret, egressImage)

	if redisEnabled {
		fmt.Fprintf(&b, `
[automation.idle_pause]
enabled = true
[automation.idle_pause.redis]
dsn = %q
`, redisDSN)
	}
	return b.String()
}

func mergeMaps(dst, src map[string]any) {
	for k, v := range src {
		if srcSub, ok := v.(map[string]any); ok {
			if dstSub, ok := dst[k].(map[string]any); ok {
				mergeMaps(dstSub, srcSub)
				continue
			}
		}
		dst[k] = v
	}
}

func (p *OverlayBDProvisioner) installHelmStack(ctx context.Context, rc *resolvedClients, prof *profile.BenchmarkProfile, opts Options, systemNS string) error {
	chartPath := strings.TrimSpace(prof.Stack.ChartPath)
	if chartPath == "" {
		chartPath = "manifests/charts/opensandbox"
	}

	values := BuildHelmValues(prof)
	rawYAML, err := yaml.Marshal(values)
	if err != nil {
		return fmt.Errorf("marshal helm values: %w", err)
	}

	tmpFile, err := os.CreateTemp("", "opensandbox-bench-values-*.yaml")
	if err != nil {
		return fmt.Errorf("create temp helm values file: %w", err)
	}
	valuesPath := tmpFile.Name()
	defer os.Remove(valuesPath)

	if _, err := tmpFile.Write(rawYAML); err != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("write temp helm values file: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("close temp helm values file: %w", err)
	}

	args := []string{
		"upgrade", "--install", p.helmReleaseName(), chartPath,
		"--namespace", systemNS,
		"--create-namespace",
		"-f", valuesPath,
		"--wait",
		"--timeout", p.readyTimeout().String(),
	}
	if opts.Kubeconfig != "" {
		args = append(args, "--kubeconfig", opts.Kubeconfig)
	}

	rc.logger.Info("installing OpenSandbox + fast-sandbox Helm stack", "release", p.helmReleaseName(), "chart", chartPath, "namespace", systemNS)
	if _, err := rc.cmd.Run(ctx, "helm", args...); err != nil {
		return fmt.Errorf("helm upgrade --install: %w", err)
	}
	return nil
}

func (p *OverlayBDProvisioner) uninstallHelmStack(ctx context.Context, rc *resolvedClients, opts Options, systemNS string) error {
	args := []string{
		"uninstall", p.helmReleaseName(),
		"--namespace", systemNS,
		"--ignore-not-found",
	}
	if opts.Kubeconfig != "" {
		args = append(args, "--kubeconfig", opts.Kubeconfig)
	}
	rc.logger.Info("uninstalling Helm release", "release", p.helmReleaseName(), "namespace", systemNS)
	if _, err := rc.cmd.Run(ctx, "helm", args...); err != nil {
		return fmt.Errorf("helm uninstall: %w", err)
	}
	return nil
}

func (p *OverlayBDProvisioner) waitForStackReady(ctx context.Context, rc *resolvedClients, prof *profile.BenchmarkProfile, systemNS string) error {
	for _, dep := range []string{"opensandbox-server", "opensandbox-ingress-gateway", "fast-sandbox-controller"} {
		if err := p.waitForDeploymentReady(ctx, rc.kube, systemNS, dep); err != nil {
			return err
		}
	}
	if err := p.waitForDaemonSetReady(ctx, rc.kube, systemNS, "firecracker-runtime"); err != nil {
		return err
	}
	if err := p.waitForFirecrackerNodesReady(ctx, rc); err != nil {
		return err
	}
	if poolLow := prof.Stack.FastSandbox.Runtime.OCI.PoolLow; poolLow > 0 {
		if err := p.waitForUblkdPoolWarm(ctx, rc, systemNS, poolLow); err != nil {
			return err
		}
	}
	return nil
}

func (p *OverlayBDProvisioner) waitForFirecrackerNodesReady(ctx context.Context, rc *resolvedClients) error {
	waitCtx, cancel := context.WithTimeout(ctx, p.readyTimeout())
	defer cancel()

	ticker := time.NewTicker(p.pollInterval())
	defer ticker.Stop()

	for {
		nodeList, err := rc.kube.CoreV1().Nodes().List(waitCtx, metav1.ListOptions{})
		if err != nil {
			return fmt.Errorf("list nodes for FirecrackerReady check: %w", err)
		}
		readyCount := 0
		for _, n := range nodeList.Items {
			if n.Labels[firecrackerNodeLabelKey] == "true" && hasFirecrackerReadyCondition(&n) {
				readyCount++
			}
		}
		if readyCount > 0 {
			return nil
		}

		select {
		case <-waitCtx.Done():
			return fmt.Errorf("timed out waiting for nodes with label %s=true and condition %s=True: %w",
				firecrackerNodeLabelKey, firecrackerReadyCondition, waitCtx.Err())
		case <-ticker.C:
		}
	}
}

func hasFirecrackerReadyCondition(node *corev1.Node) bool {
	for _, cond := range node.Status.Conditions {
		if cond.Type == firecrackerReadyCondition && cond.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

type ublkdPoolStatus struct {
	Idle   int `json:"idle"`
	Active int `json:"active"`
	Hits   int `json:"hits"`
	Misses int `json:"misses"`
}

func (p *OverlayBDProvisioner) waitForUblkdPoolWarm(ctx context.Context, rc *resolvedClients, systemNS string, poolLow int) error {
	if rc.execer == nil {
		return fmt.Errorf("cannot verify overlaybd-ublkd /v1/pool readiness: pod execer is nil")
	}

	waitCtx, cancel := context.WithTimeout(ctx, p.readyTimeout())
	defer cancel()

	ticker := time.NewTicker(p.pollInterval())
	defer ticker.Stop()

	probeCmd := []string{"sh", "-ec", fmt.Sprintf("curl -fsS --max-time 2 --unix-socket %s http://localhost/v1/pool", defaultUblkdSocketPath)}

	for {
		pods, err := rc.kube.CoreV1().Pods(systemNS).List(waitCtx, metav1.ListOptions{
			LabelSelector: "app=fast-sandbox,component=firecracker-runtime",
		})
		if err != nil {
			return fmt.Errorf("list firecracker-runtime pods: %w", err)
		}
		if len(pods.Items) > 0 {
			allWarm := true
			for _, pod := range pods.Items {
				if pod.Status.Phase != corev1.PodRunning || !isPodReady(&pod) {
					allWarm = false
					break
				}
				stdout, _, execErr := rc.execer.Exec(waitCtx, systemNS, pod.Name, "overlaybd-ublkd", probeCmd)
				if execErr != nil {
					allWarm = false
					break
				}
				var st ublkdPoolStatus
				if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &st); err != nil || st.Idle < poolLow {
					allWarm = false
					break
				}
			}
			if allWarm {
				return nil
			}
		}

		select {
		case <-waitCtx.Done():
			return fmt.Errorf("timed out waiting for overlaybd-ublkd /v1/pool idle >= %d across firecracker-runtime pods: %w", poolLow, waitCtx.Err())
		case <-ticker.C:
		}
	}
}
