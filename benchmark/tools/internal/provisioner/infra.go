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
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"

	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/profile"
)

func (p *OverlayBDProvisioner) ensureRunnerRBAC(ctx context.Context, client kubernetes.Interface, benchNS string) error {
	labels := map[string]string{
		"app.kubernetes.io/name": runnerServiceAccountName,
		managedByLabelKey:        managedByLabelValue,
	}

	if err := upsertServiceAccount(ctx, client, &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      runnerServiceAccountName,
			Namespace: benchNS,
			Labels:    labels,
		},
	}); err != nil {
		return err
	}

	cr := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{
			Name:   runnerClusterRoleName,
			Labels: labels,
		},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{""},
				Resources: []string{"nodes/status", "nodes/proxy", "pods/log", "services", "endpoints", "configmaps", "namespaces"},
				Verbs:     []string{"get", "list", "watch"},
			},
			{
				APIGroups: []string{""},
				Resources: []string{"nodes"},
				Verbs:     []string{"get", "list", "watch", "patch"},
			},
			{
				APIGroups: []string{""},
				Resources: []string{"pods"},
				Verbs:     []string{"get", "list", "watch", "delete"},
			},
			{
				APIGroups: []string{""},
				Resources: []string{"pods/exec"},
				Verbs:     []string{"create", "get"},
			},
			{
				APIGroups: []string{"apps"},
				Resources: []string{"deployments", "daemonsets"},
				Verbs:     []string{"get", "list", "watch"},
			},
			{
				APIGroups: []string{"sandbox.fast.io"},
				Resources: []string{"sandboxes", "sandboxsnapshots", "sandboxpools", "sandboxtemplates"},
				Verbs:     []string{"get", "list", "watch", "create", "update", "patch", "delete"},
			},
		},
	}
	if err := upsertClusterRole(ctx, client, cr); err != nil {
		return err
	}

	crb := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:   runnerClusterRoleName,
			Labels: labels,
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "ClusterRole",
			Name:     runnerClusterRoleName,
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      rbacv1.ServiceAccountKind,
				Name:      runnerServiceAccountName,
				Namespace: benchNS,
			},
		},
	}
	return upsertClusterRoleBinding(ctx, client, crb)
}

// ensureFastSandboxLoggingConfig creates the optional fast-sandbox-logging ConfigMap
// with alsoStderr: true in each target namespace so fast-sandbox components (controller,
// firecracker-runtime, janitor, fastlet, fastlet-proxy, sandboxtemplate-builder) mirror
// application logs to container stderr/stdout for `kubectl logs`.
func (p *OverlayBDProvisioner) ensureFastSandboxLoggingConfig(ctx context.Context, client kubernetes.Interface, namespaces ...string) error {
	const loggingYAML = "directory: /var/log/fast-sandbox\nmaxSizeMB: 100\nmaxBackups: 10\nmaxAgeDays: 7\ncompress: true\nalsoStderr: true\n"
	labels := map[string]string{
		managedByLabelKey: managedByLabelValue,
	}
	for _, ns := range uniqueStrings(namespaces...) {
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fastSandboxLoggingConfigMapName,
				Namespace: ns,
				Labels:    labels,
			},
			Data: map[string]string{
				"logging.yaml": loggingYAML,
			},
		}
		if err := upsertConfigMap(ctx, client, cm); err != nil {
			return err
		}
	}
	return nil
}

// ensureSecureAccessSecret creates the OSEP-0011 route-token signing Secret shared between
// opensandbox-server and opensandbox-ingress-gateway in systemNS if it does not already exist.
func (p *OverlayBDProvisioner) ensureSecureAccessSecret(ctx context.Context, client kubernetes.Interface, systemNS string) error {
	if _, err := client.CoreV1().Secrets(systemNS).Get(ctx, secureAccessSecretName, metav1.GetOptions{}); err == nil {
		return nil
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get secret %s/%s: %w", systemNS, secureAccessSecretName, err)
	}

	rawKey := make([]byte, 32)
	if _, err := rand.Read(rawKey); err != nil {
		return fmt.Errorf("generate secure-access key: %w", err)
	}
	keyB64 := base64.StdEncoding.EncodeToString(rawKey)

	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secureAccessSecretName,
			Namespace: systemNS,
			Labels: map[string]string{
				managedByLabelKey: managedByLabelValue,
			},
		},
		Type: corev1.SecretTypeOpaque,
		StringData: map[string]string{
			"keys":       "a=" + keyB64,
			"active-key": "a",
		},
	}
	if _, err := client.CoreV1().Secrets(systemNS).Create(ctx, sec, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create secret %s/%s: %w", systemNS, secureAccessSecretName, err)
	}
	return nil
}

// ensureFastSandboxRegistryConfig creates/updates the fast-sandbox-registry ConfigMap
// (registries.yaml) in resourceNS so fastlets can pull OCI templates and push/pull
// pause/resume & rootfs-only snapshots using the configured publish registry Secret.
func (p *OverlayBDProvisioner) ensureFastSandboxRegistryConfig(ctx context.Context, client kubernetes.Interface, prof *profile.BenchmarkProfile, resourceNS, benchNS string) error {
	publishTarget := strings.TrimSpace(prof.Stack.Template.Publish)
	if publishTarget == "" {
		if !prof.Environment.Registry.EphemeralInCluster {
			return nil
		}
		publishTarget = fmt.Sprintf("oci://%s/opensandbox/templates", ephemeralRegistryHost(benchNS))
	}
	raw := strings.TrimPrefix(publishTarget, "oci://")
	host, repoPath, ok := strings.Cut(raw, "/")
	if !ok || strings.TrimSpace(host) == "" {
		return fmt.Errorf("invalid OCI publish target %q: expected oci://<host>/<repo>", publishTarget)
	}
	repoPrefix, _, _ := strings.Cut(strings.TrimPrefix(repoPath, "/"), "/")
	secretName := strings.TrimSpace(prof.Stack.Template.PublishSecretRef)
	if secretName == "" {
		secretName = ociPublishSecretName
	}

	var b strings.Builder
	b.WriteString("registries:\n")
	fmt.Fprintf(&b, "  - host: %s\n", host)
	if repoPrefix != "" {
		fmt.Fprintf(&b, "    repositoryPrefix: %s\n", repoPrefix)
	}
	b.WriteString("    secretRef:\n")
	fmt.Fprintf(&b, "      name: %s\n", secretName)

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fastSandboxRegistryConfigMapName,
			Namespace: resourceNS,
			Labels: map[string]string{
				managedByLabelKey: managedByLabelValue,
			},
		},
		Data: map[string]string{
			"registries.yaml": b.String(),
		},
	}
	return upsertConfigMap(ctx, client, cm)
}

// ensureAgentRegistrySecretIfMissing ensures the Secret mounted by firecracker-runtime
// (fast-sandbox-agent-registry) exists in systemNS when ephemeralInCluster is false so
// firecracker-runtime pods do not hang in Init:0/1 after a prior env down.
func (p *OverlayBDProvisioner) ensureAgentRegistrySecretIfMissing(ctx context.Context, client kubernetes.Interface, systemNS string) error {
	if _, err := client.CoreV1().Secrets(systemNS).Get(ctx, agentRegistrySecretName, metav1.GetOptions{}); err == nil {
		return nil
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get secret %s/%s: %w", systemNS, agentRegistrySecretName, err)
	}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      agentRegistrySecretName,
			Namespace: systemNS,
			Labels: map[string]string{
				managedByLabelKey: managedByLabelValue,
			},
		},
		Type: corev1.SecretTypeOpaque,
		StringData: map[string]string{
			"registry.json": "{\"registries\":[]}\n",
		},
	}
	if _, err := client.CoreV1().Secrets(systemNS).Create(ctx, sec, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create secret %s/%s: %w", systemNS, agentRegistrySecretName, err)
	}
	return nil
}

func (p *OverlayBDProvisioner) prepareNodes(ctx context.Context, rc *resolvedClients, prof *profile.BenchmarkProfile, benchNS string) error {
	nodeList, err := rc.kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list nodes for prepare-nodes: %w", err)
	}
	candidates := selectCandidateNodes(nodeList.Items)
	if len(candidates) == 0 {
		return fmt.Errorf("prepare-nodes: no Ready candidate nodes found")
	}

	probeImage := resolveProbeImage(prof)
	for _, node := range candidates {
		jobName := fmt.Sprintf("osb-prep-%s", sanitizeDNSLabel(node.Name, 40))
		job := buildPrepareNodeJob(benchNS, jobName, node.Name, probeImage, defaultStateRoot)

		// Delete any previous job with the same name.
		prop := metav1.DeletePropagationBackground
		_ = rc.kube.BatchV1().Jobs(benchNS).Delete(ctx, jobName, metav1.DeleteOptions{PropagationPolicy: &prop})

		rc.logger.Info("launching host preparation job", "node", node.Name, "job", jobName)
		if _, err := rc.kube.BatchV1().Jobs(benchNS).Create(ctx, job, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("create prepare-nodes job %s/%s: %w", benchNS, jobName, err)
		}
		if err := p.waitForJobComplete(ctx, rc.kube, benchNS, jobName); err != nil {
			return fmt.Errorf("prepare-nodes on %q: %w", node.Name, err)
		}
		_ = rc.kube.BatchV1().Jobs(benchNS).Delete(ctx, jobName, metav1.DeleteOptions{PropagationPolicy: &prop})
	}
	return nil
}

func buildPrepareNodeJob(namespace, jobName, nodeName, image, stateRoot string) *batchv1.Job {
	script := fmt.Sprintf(`set -eu
nsenter -t 1 -m -u -i -n -- sh -eu -c '
  if [ ! -c /dev/ublk-control ]; then
    modprobe ublk_drv
  fi
  mkdir -p %s
  SRC="%s/.osb-reflink-check-$$"
  DST="%s/.osb-reflink-check-dst-$$"
  if ! (touch "$SRC" && cp --reflink=always "$SRC" "$DST" 2>/dev/null); then
    rm -f "$SRC" "$DST"
    IMG="/var/lib/opensandbox-bench-stateroot.xfs"
    if ! mountpoint -q %s; then
      if [ ! -f "$IMG" ]; then
        truncate -s 64G "$IMG"
        mkfs.xfs -m reflink=1 "$IMG"
      fi
      mount -o loop "$IMG" %s
    fi
  else
    rm -f "$SRC" "$DST"
  fi
'
`, stateRoot, stateRoot, stateRoot, stateRoot, stateRoot)

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name": "opensandbox-bench-prepare-node",
				managedByLabelKey:        managedByLabelValue,
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: int32Ptr(0),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app.kubernetes.io/name": "opensandbox-bench-prepare-node",
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
							Name:    "prepare",
							Image:   image,
							Command: []string{"sh", "-c", script},
							SecurityContext: &corev1.SecurityContext{
								Privileged: boolPtr(true),
							},
						},
					},
				},
			},
		},
	}
}

func (p *OverlayBDProvisioner) waitForJobComplete(ctx context.Context, client kubernetes.Interface, namespace, jobName string) error {
	waitCtx, cancel := context.WithTimeout(ctx, p.readyTimeout())
	defer cancel()

	ticker := time.NewTicker(p.pollInterval())
	defer ticker.Stop()

	for {
		job, err := client.BatchV1().Jobs(namespace).Get(waitCtx, jobName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("poll job %s/%s: %w", namespace, jobName, err)
		}
		for _, cond := range job.Status.Conditions {
			if cond.Type == batchv1.JobComplete && cond.Status == corev1.ConditionTrue {
				return nil
			}
			if cond.Type == batchv1.JobFailed && cond.Status == corev1.ConditionTrue {
				return fmt.Errorf("job %s/%s failed: %s", namespace, jobName, cond.Message)
			}
		}
		if job.Status.Succeeded > 0 {
			return nil
		}
		if job.Status.Failed > 0 {
			return fmt.Errorf("job %s/%s failed (%d failed pods)", namespace, jobName, job.Status.Failed)
		}

		select {
		case <-waitCtx.Done():
			return fmt.Errorf("timed out waiting for job %s/%s: %w", namespace, jobName, waitCtx.Err())
		case <-ticker.C:
		}
	}
}

func ephemeralRegistryHost(benchNS string) string {
	return fmt.Sprintf("%s.%s.svc:%d", ephemeralRegistryName, benchNS, ephemeralRegistryPort)
}

func (p *OverlayBDProvisioner) deployEphemeralRegistry(ctx context.Context, rc *resolvedClients, prof *profile.BenchmarkProfile, systemNS, resourceNS, benchNS string) error {
	bundle, err := GenerateRegistryCertBundle(ephemeralRegistryName, benchNS, 7*24*time.Hour)
	if err != nil {
		return err
	}

	labels := map[string]string{
		"app.kubernetes.io/name": ephemeralRegistryName,
		managedByLabelKey:        managedByLabelValue,
	}

	// TLS secret in benchNamespace for registry:2 server.
	if err := upsertSecret(ctx, rc.kube, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ephemeralRegistryTLSSecret,
			Namespace: benchNS,
			Labels:    labels,
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey:       bundle.ServerCertPEM,
			corev1.TLSPrivateKeyKey: bundle.ServerKeyPEM,
			"ca.crt":                bundle.CACertPEM,
		},
	}); err != nil {
		return err
	}

	// CA bundle secret distributed across bench, system, and resource namespaces.
	seenNS := map[string]bool{}
	for _, ns := range []string{benchNS, systemNS, resourceNS} {
		if seenNS[ns] {
			continue
		}
		seenNS[ns] = true
		if err := upsertSecret(ctx, rc.kube, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      ephemeralRegistryCASecret,
				Namespace: ns,
				Labels:    labels,
			},
			Type: corev1.SecretTypeOpaque,
			Data: map[string][]byte{
				"ca.crt": bundle.CACertPEM,
			},
		}); err != nil {
			return err
		}
	}

	regHost := ephemeralRegistryHost(benchNS)
	agentRegJSON, err := buildAgentRegistryJSON("https://" + regHost)
	if err != nil {
		return fmt.Errorf("marshal fast-sandbox-agent-registry payload: %w", err)
	}
	if err := upsertSecret(ctx, rc.kube, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      agentRegistrySecretName,
			Namespace: systemNS,
			Labels:    labels,
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"registry.json": agentRegJSON,
		},
	}); err != nil {
		return err
	}

	dockerCfg, err := buildDockerConfigJSON(regHost)
	if err != nil {
		return fmt.Errorf("marshal dockerconfigjson: %w", err)
	}
	for _, ns := range uniqueStrings(resourceNS, systemNS) {
		if err := upsertSecret(ctx, rc.kube, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      ociPublishSecretName,
				Namespace: ns,
				Labels:    labels,
			},
			Type: corev1.SecretTypeDockerConfigJson,
			Data: map[string][]byte{
				corev1.DockerConfigJsonKey: dockerCfg,
			},
		}); err != nil {
			return err
		}
	}

	// Storage volume: hostPath if configured, else PVC sized per profile.registry.storageSize.
	var dataVolume corev1.Volume
	if hp := strings.TrimSpace(prof.Environment.Registry.HostPath); hp != "" {
		dirOrCreate := corev1.HostPathDirectoryOrCreate
		dataVolume = corev1.Volume{
			Name: "registry-data",
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: hp,
					Type: &dirOrCreate,
				},
			},
		}
	} else {
		storageSize := strings.TrimSpace(prof.Environment.Registry.StorageSize)
		if storageSize == "" {
			storageSize = "200Gi"
		}
		qty, err := resource.ParseQuantity(storageSize)
		if err != nil {
			return fmt.Errorf("parse registry.storageSize %q: %w", storageSize, err)
		}
		pvcName := ephemeralRegistryName + "-data"
		pvc := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      pvcName,
				Namespace: benchNS,
				Labels:    labels,
			},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceStorage: qty,
					},
				},
			},
		}
		if sc := strings.TrimSpace(prof.Environment.Registry.StorageClass); sc != "" {
			pvc.Spec.StorageClassName = &sc
		}
		if err := ensurePVC(ctx, rc.kube, pvc); err != nil {
			return err
		}
		dataVolume = corev1.Volume{
			Name: "registry-data",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: pvcName,
				},
			},
		}
	}

	regImage := strings.TrimSpace(prof.Environment.Registry.Image)
	if regImage == "" {
		regImage = "registry:2"
	}
	regImage = applyImageMirror(prof, regImage)

	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ephemeralRegistryName,
			Namespace: benchNS,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(1),
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": ephemeralRegistryName}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					Affinity: nonFirecrackerNodeAffinity(),
					Containers: []corev1.Container{
						{
							Name:            "registry",
							Image:           regImage,
							ImagePullPolicy: corev1.PullIfNotPresent,
							Ports: []corev1.ContainerPort{
								{Name: "https", ContainerPort: ephemeralRegistryPort, Protocol: corev1.ProtocolTCP},
							},
							Env: []corev1.EnvVar{
								{Name: "REGISTRY_STORAGE_DELETE_ENABLED", Value: "true"},
								{Name: "REGISTRY_HTTP_ADDR", Value: fmt.Sprintf("0.0.0.0:%d", ephemeralRegistryPort)},
								{Name: "REGISTRY_HTTP_TLS_CERTIFICATE", Value: "/certs/tls.crt"},
								{Name: "REGISTRY_HTTP_TLS_KEY", Value: "/certs/tls.key"},
							},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "registry-certs", MountPath: "/certs", ReadOnly: true},
								{Name: "registry-data", MountPath: "/var/lib/registry"},
							},
							ReadinessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(ephemeralRegistryPort)},
								},
								InitialDelaySeconds: 2,
								PeriodSeconds:       5,
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "registry-certs",
							VolumeSource: corev1.VolumeSource{
								Secret: &corev1.SecretVolumeSource{SecretName: ephemeralRegistryTLSSecret},
							},
						},
						dataVolume,
					},
				},
			},
		},
	}
	if err := upsertDeployment(ctx, rc.kube, dep); err != nil {
		return err
	}

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ephemeralRegistryName,
			Namespace: benchNS,
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: map[string]string{"app.kubernetes.io/name": ephemeralRegistryName},
			Ports: []corev1.ServicePort{
				{
					Name:       "https",
					Port:       ephemeralRegistryPort,
					TargetPort: intstr.FromInt32(ephemeralRegistryPort),
					Protocol:   corev1.ProtocolTCP,
				},
			},
		},
	}
	if err := upsertService(ctx, rc.kube, svc); err != nil {
		return err
	}

	return p.waitForDeploymentReady(ctx, rc.kube, benchNS, ephemeralRegistryName)
}

func (p *OverlayBDProvisioner) deployRedis(ctx context.Context, rc *resolvedClients, prof *profile.BenchmarkProfile, benchNS string) error {
	labels := map[string]string{
		"app.kubernetes.io/name": ephemeralRedisName,
		managedByLabelKey:        managedByLabelValue,
	}
	redisImage := applyImageMirror(prof, "redis:7-alpine")

	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ephemeralRedisName,
			Namespace: benchNS,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(1),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": ephemeralRedisName}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					Affinity: nonFirecrackerNodeAffinity(),
					Containers: []corev1.Container{
						{
							Name:            "redis",
							Image:           redisImage,
							ImagePullPolicy: corev1.PullIfNotPresent,
							Args:            []string{"--save", "", "--appendonly", "no"},
							Ports: []corev1.ContainerPort{
								{Name: "redis", ContainerPort: ephemeralRedisPort, Protocol: corev1.ProtocolTCP},
							},
							ReadinessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(ephemeralRedisPort)},
								},
								InitialDelaySeconds: 2,
								PeriodSeconds:       5,
							},
						},
					},
				},
			},
		},
	}
	if err := upsertDeployment(ctx, rc.kube, dep); err != nil {
		return err
	}

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ephemeralRedisName,
			Namespace: benchNS,
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: map[string]string{"app.kubernetes.io/name": ephemeralRedisName},
			Ports: []corev1.ServicePort{
				{
					Name:       "redis",
					Port:       ephemeralRedisPort,
					TargetPort: intstr.FromInt32(ephemeralRedisPort),
					Protocol:   corev1.ProtocolTCP,
				},
			},
		},
	}
	if err := upsertService(ctx, rc.kube, svc); err != nil {
		return err
	}

	return p.waitForDeploymentReady(ctx, rc.kube, benchNS, ephemeralRedisName)
}

func (p *OverlayBDProvisioner) waitForDeploymentReady(ctx context.Context, client kubernetes.Interface, namespace, name string) error {
	waitCtx, cancel := context.WithTimeout(ctx, p.readyTimeout())
	defer cancel()

	ticker := time.NewTicker(p.pollInterval())
	defer ticker.Stop()

	for {
		dep, err := client.AppsV1().Deployments(namespace).Get(waitCtx, name, metav1.GetOptions{})
		if err == nil {
			desired := int32(1)
			if dep.Spec.Replicas != nil {
				desired = *dep.Spec.Replicas
			}
			if dep.Status.ReadyReplicas >= desired && dep.Status.AvailableReplicas >= desired {
				return nil
			}
		} else if !apierrors.IsNotFound(err) {
			return fmt.Errorf("poll deployment %s/%s: %w", namespace, name, err)
		}

		select {
		case <-waitCtx.Done():
			return fmt.Errorf("timed out waiting for deployment %s/%s to become Ready: %w", namespace, name, waitCtx.Err())
		case <-ticker.C:
		}
	}
}

func (p *OverlayBDProvisioner) waitForDaemonSetReady(ctx context.Context, client kubernetes.Interface, namespace, name string) error {
	waitCtx, cancel := context.WithTimeout(ctx, p.readyTimeout())
	defer cancel()

	ticker := time.NewTicker(p.pollInterval())
	defer ticker.Stop()

	for {
		ds, err := client.AppsV1().DaemonSets(namespace).Get(waitCtx, name, metav1.GetOptions{})
		if err == nil {
			if ds.Status.DesiredNumberScheduled > 0 && ds.Status.NumberReady >= ds.Status.DesiredNumberScheduled {
				return nil
			}
		} else if !apierrors.IsNotFound(err) {
			return fmt.Errorf("poll daemonset %s/%s: %w", namespace, name, err)
		}

		select {
		case <-waitCtx.Done():
			return fmt.Errorf("timed out waiting for daemonset %s/%s to become Ready: %w", namespace, name, waitCtx.Err())
		case <-ticker.C:
		}
	}
}

func uniqueStrings(items ...string) []string {
	seen := make(map[string]bool, len(items))
	out := make([]string, 0, len(items))
	for _, s := range items {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
