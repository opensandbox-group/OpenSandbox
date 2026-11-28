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
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

const (
	RolloutScenarioPool         = "pool_rollout"
	RolloutScenarioMultiFastlet = "multi_fastlet_colocation"
	RolloutScenarioDaemonSet    = "daemonset_rollout"

	rolloutTriggerAnnotationKey = "bench.opensandbox.io/rollout-trigger"
	rolloutTriggerEnvName       = "BENCH_ROLLOUT_TRIGGER"
	colocationTempLabelKey      = "bench.opensandbox.io/colocation-temp"
)

// RolloutTriggerOptions configures cluster rollout disruption operations.
type RolloutTriggerOptions struct {
	PoolName       string
	DaemonSetName  string
	Timeout        time.Duration
	ColocationHold time.Duration
	PollInterval   time.Duration
}

// RolloutTriggerResult summarizes a completed or timed-out rollout trigger operation.
type RolloutTriggerResult struct {
	Scenario         string   `json:"scenario"`
	Target           string   `json:"target"`
	TargetNode       string   `json:"target_node,omitempty"`
	DurationSeconds  float64  `json:"duration_seconds"`
	AffectedPods     []string `json:"affected_pods,omitempty"`
	RolloutCompleted bool     `json:"rollout_completed"`
	Detail           string   `json:"detail,omitempty"`
}

// TriggerRollout executes one of the three rollout disruption scenarios:
//   - pool_rollout: triggers a SandboxPool rolling upgrade via template annotation & env update
//   - multi_fastlet_colocation: temporarily deploys a sibling SandboxPool pinned to an active node
//     without podAntiAffinity to exercise shared hostPath cache/GC contention, then cleans it up
//   - daemonset_rollout: triggers a rolling restart of the firecracker-runtime DaemonSet
func (c *ClusterOps) TriggerRollout(ctx context.Context, scenario string, opts RolloutTriggerOptions) (*RolloutTriggerResult, error) {
	if opts.Timeout <= 0 {
		opts.Timeout = 3 * time.Minute
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = 1 * time.Second
	}
	if opts.ColocationHold <= 0 {
		opts.ColocationHold = 10 * time.Second
	}

	switch scenario {
	case RolloutScenarioPool:
		return c.TriggerFastletPoolRollout(ctx, opts)
	case RolloutScenarioMultiFastlet:
		return c.TriggerMultiFastletColocation(ctx, opts)
	case RolloutScenarioDaemonSet:
		return c.TriggerRuntimeDaemonSetRollout(ctx, opts)
	default:
		return nil, fmt.Errorf("unsupported rollout scenario %q", scenario)
	}
}

// TriggerFastletPoolRollout updates the SandboxPool fastletTemplate to trigger a rolling
// replacement of Fastlet Pods and waits for the replacement Pods to become Ready.
func (c *ClusterOps) TriggerFastletPoolRollout(ctx context.Context, opts RolloutTriggerOptions) (*RolloutTriggerResult, error) {
	if c.clients == nil || c.clients.Clientset == nil || c.clients.DynamicClient == nil {
		return nil, fmt.Errorf("TriggerFastletPoolRollout requires kubernetes clientset and dynamic client")
	}

	start := time.Now()
	poolObj, gvr, err := c.resolveSandboxPool(ctx, opts.PoolName)
	if err != nil {
		return nil, err
	}
	poolName := poolObj.GetName()
	ns := poolObj.GetNamespace()
	if ns == "" {
		ns = c.cfg.ResourceNamespace
	}

	res := &RolloutTriggerResult{
		Scenario: RolloutScenarioPool,
		Target:   fmt.Sprintf("%s/%s", ns, poolName),
	}

	oldPods, err := c.listPoolFastletPods(ctx, ns, poolName)
	if err != nil {
		return res, fmt.Errorf("list fastlet pods for pool %s/%s: %w", ns, poolName, err)
	}
	oldUIDs := make(map[types.UID]string, len(oldPods))
	for _, p := range oldPods {
		oldUIDs[p.UID] = p.Name
		res.AffectedPods = append(res.AffectedPods, p.Name)
	}
	sort.Strings(res.AffectedPods)

	expectedReady := len(oldPods)
	if expectedReady == 0 {
		if minVal, found, _ := unstructured.NestedInt64(poolObj.Object, "spec", "capacity", "poolMin"); found && minVal > 0 {
			expectedReady = int(minVal)
		} else {
			expectedReady = 1
		}
	}

	token := fmt.Sprintf("bench-%d", time.Now().UTC().UnixNano())
	origTemplate, hasOrigTemplate, _ := unstructured.NestedMap(poolObj.Object, "spec", "fastletTemplate")
	if hasOrigTemplate && origTemplate != nil {
		origTemplate = runtimeDeepCopyMap(origTemplate)
	}
	if err := c.stampSandboxPoolTemplate(ctx, gvr, ns, poolObj, token); err != nil {
		res.DurationSeconds = time.Since(start).Seconds()
		return res, fmt.Errorf("stamp SandboxPool %s/%s for rollout: %w", ns, poolName, err)
	}

	waitCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	for {
		curPods, listErr := c.listPoolFastletPods(waitCtx, ns, poolName)
		if listErr == nil {
			oldRemaining := 0
			newReady := 0
			var newNames []string
			for i := range curPods {
				p := &curPods[i]
				if _, isOld := oldUIDs[p.UID]; isOld {
					oldRemaining++
					continue
				}
				if p.DeletionTimestamp == nil && isPodContainersReady(p) {
					newReady++
					newNames = append(newNames, p.Name)
				}
			}
			if oldRemaining == 0 && newReady >= expectedReady {
				sort.Strings(newNames)
				res.RolloutCompleted = true
				res.DurationSeconds = time.Since(start).Seconds()
				res.Detail = fmt.Sprintf("replaced %d fastlet pod(s) with %d ready pod(s): %s", len(oldUIDs), newReady, strings.Join(newNames, ","))
				return res, nil
			}
		}

		select {
		case <-waitCtx.Done():
			res.DurationSeconds = time.Since(start).Seconds()
			diag := c.diagnosePoolRolloutStall(ns, poolName, oldUIDs)
			if hasOrigTemplate {
				if rbErr := c.restoreSandboxPoolTemplate(gvr, ns, poolName, origTemplate); rbErr != nil {
					diag += fmt.Sprintf("; rollback failed: %v", rbErr)
				} else {
					diag += "; rolled back fastletTemplate"
				}
			}
			res.Detail = fmt.Sprintf("timed out after %.1fs waiting for pool %s/%s rollout (%s)", res.DurationSeconds, ns, poolName, diag)
			return res, fmt.Errorf("pool rollout timeout (%s/%s: %s): %w", ns, poolName, diag, waitCtx.Err())
		case <-time.After(opts.PollInterval):
		}
	}
}

func (c *ClusterOps) diagnosePoolRolloutStall(ns, poolName string, oldUIDs map[types.UID]string) string {
	diagCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pods, err := c.listPoolFastletPods(diagCtx, ns, poolName)
	if err != nil {
		return fmt.Sprintf("list pods failed: %v", err)
	}
	var oldRemaining, newPending, newReady int
	var unschedReason string
	for i := range pods {
		p := &pods[i]
		if _, isOld := oldUIDs[p.UID]; isOld {
			oldRemaining++
			continue
		}
		if isPodContainersReady(p) {
			newReady++
			continue
		}
		if p.Status.Phase == corev1.PodPending {
			newPending++
			for _, cond := range p.Status.Conditions {
				if cond.Type == corev1.PodScheduled && cond.Status == corev1.ConditionFalse && unschedReason == "" {
					unschedReason = strings.TrimSpace(cond.Reason + ": " + cond.Message)
				}
			}
		}
	}
	msg := fmt.Sprintf("old_remaining=%d, new_ready=%d, new_pending=%d", oldRemaining, newReady, newPending)
	if unschedReason != "" {
		msg += fmt.Sprintf(", unschedulable=(%s)", unschedReason)
	}
	return msg
}

func (c *ClusterOps) restoreSandboxPoolTemplate(gvr schema.GroupVersionResource, ns, poolName string, origTemplate map[string]any) error {
	rbCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cur, err := c.clients.DynamicClient.Resource(gvr).Namespace(ns).Get(rbCtx, poolName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	updated := cur.DeepCopy()
	if err := unstructured.SetNestedMap(updated.Object, origTemplate, "spec", "fastletTemplate"); err != nil {
		return err
	}
	_, err = c.clients.DynamicClient.Resource(gvr).Namespace(ns).Update(rbCtx, updated, metav1.UpdateOptions{})
	return err
}

// TriggerMultiFastletColocation creates a temporary single-replica sibling SandboxPool pinned
// to a node already running a Fastlet Pod (with podAntiAffinity removed), waits for the
// co-located Fastlet Pod to become Ready, holds for ColocationHold, and cleanly deletes it.
func (c *ClusterOps) TriggerMultiFastletColocation(ctx context.Context, opts RolloutTriggerOptions) (*RolloutTriggerResult, error) {
	if c.clients == nil || c.clients.Clientset == nil || c.clients.DynamicClient == nil {
		return nil, fmt.Errorf("TriggerMultiFastletColocation requires kubernetes clientset and dynamic client")
	}

	start := time.Now()
	poolObj, gvr, err := c.resolveSandboxPool(ctx, opts.PoolName)
	if err != nil {
		return nil, err
	}
	poolName := poolObj.GetName()
	ns := poolObj.GetNamespace()
	if ns == "" {
		ns = c.cfg.ResourceNamespace
	}

	primaryPods, err := c.listPoolFastletPods(ctx, ns, poolName)
	if err != nil {
		return nil, fmt.Errorf("list fastlet pods for pool %s/%s: %w", ns, poolName, err)
	}
	var targetNode string
	for _, p := range primaryPods {
		if p.DeletionTimestamp == nil && strings.TrimSpace(p.Spec.NodeName) != "" && isPodContainersReady(&p) {
			targetNode = p.Spec.NodeName
			break
		}
	}
	if targetNode == "" {
		for _, p := range primaryPods {
			if p.DeletionTimestamp == nil && strings.TrimSpace(p.Spec.NodeName) != "" {
				targetNode = p.Spec.NodeName
				break
			}
		}
	}
	if targetNode == "" {
		return nil, fmt.Errorf("no scheduled fastlet pod found for pool %s/%s to co-locate against", ns, poolName)
	}

	colocName := buildColocationPoolName(poolName, time.Now().UTC().UnixNano())
	res := &RolloutTriggerResult{
		Scenario:   RolloutScenarioMultiFastlet,
		Target:     fmt.Sprintf("%s/%s", ns, colocName),
		TargetNode: targetNode,
	}

	colocPool := buildColocationPoolObject(poolObj, colocName, ns, targetNode)
	if _, err := c.clients.DynamicClient.Resource(gvr).Namespace(ns).Create(ctx, colocPool, metav1.CreateOptions{}); err != nil {
		res.DurationSeconds = time.Since(start).Seconds()
		return res, fmt.Errorf("create temporary co-located SandboxPool %s/%s: %w", ns, colocName, err)
	}

	cleanedUp := false
	cleanup := func() error {
		if cleanedUp {
			return nil
		}
		cleanedUp = true
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cleanCancel()

		if delErr := c.clients.DynamicClient.Resource(gvr).Namespace(ns).Delete(cleanCtx, colocName, metav1.DeleteOptions{}); delErr != nil && !apierrors.IsNotFound(delErr) {
			return delErr
		}
		for {
			pods, lErr := c.listPoolFastletPods(cleanCtx, ns, colocName)
			if lErr == nil && len(pods) == 0 {
				return nil
			}
			select {
			case <-cleanCtx.Done():
				return cleanCtx.Err()
			case <-time.After(minDuration(opts.PollInterval, 500*time.Millisecond)):
			}
		}
	}
	defer func() {
		_ = cleanup()
	}()

	waitCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	var colocPodName string
	for {
		pods, listErr := c.listPoolFastletPods(waitCtx, ns, colocName)
		if listErr == nil {
			for i := range pods {
				p := &pods[i]
				if p.DeletionTimestamp == nil && isPodContainersReady(p) {
					colocPodName = p.Name
					if p.Spec.NodeName != "" {
						res.TargetNode = p.Spec.NodeName
					}
					break
				}
			}
		}
		if colocPodName != "" {
			res.AffectedPods = []string{colocPodName}
			break
		}
		select {
		case <-waitCtx.Done():
			res.DurationSeconds = time.Since(start).Seconds()
			res.Detail = fmt.Sprintf("timed out waiting for co-located fastlet pod for %s/%s on node %s", ns, colocName, targetNode)
			return res, fmt.Errorf("co-located fastlet readiness timeout (%s/%s on %s): %w", ns, colocName, targetNode, waitCtx.Err())
		case <-time.After(opts.PollInterval):
		}
	}

	// Hold the co-located Fastlet briefly while background traffic exercises the primary pool.
	holdTimer := time.NewTimer(opts.ColocationHold)
	select {
	case <-ctx.Done():
		holdTimer.Stop()
		res.DurationSeconds = time.Since(start).Seconds()
		return res, ctx.Err()
	case <-holdTimer.C:
	}

	if err := cleanup(); err != nil {
		res.DurationSeconds = time.Since(start).Seconds()
		res.Detail = fmt.Sprintf("co-located fastlet %s started on %s, cleanup error: %v", colocPodName, res.TargetNode, err)
		return res, fmt.Errorf("cleanup temporary co-located pool %s/%s: %w", ns, colocName, err)
	}

	res.RolloutCompleted = true
	res.DurationSeconds = time.Since(start).Seconds()
	res.Detail = fmt.Sprintf("co-located fastlet %s ran on node %s for %s and was cleaned up", colocPodName, res.TargetNode, opts.ColocationHold)
	return res, nil
}

// TriggerRuntimeDaemonSetRollout patches the firecracker-runtime DaemonSet template annotation
// to trigger a Kubernetes RollingUpdate and waits until all DaemonSet pods are updated and Ready.
func (c *ClusterOps) TriggerRuntimeDaemonSetRollout(ctx context.Context, opts RolloutTriggerOptions) (*RolloutTriggerResult, error) {
	if c.clients == nil || c.clients.Clientset == nil {
		return nil, fmt.Errorf("TriggerRuntimeDaemonSetRollout requires kubernetes clientset")
	}

	start := time.Now()
	dsName := strings.TrimSpace(opts.DaemonSetName)
	if dsName == "" {
		dsName = "firecracker-runtime"
	}
	ns := c.cfg.SystemNamespace
	if ns == "" {
		ns = DefaultSystemNamespace
	}

	res := &RolloutTriggerResult{
		Scenario: RolloutScenarioDaemonSet,
		Target:   fmt.Sprintf("%s/%s", ns, dsName),
	}

	oldPods, _ := listFirecrackerRuntimePods(ctx, c.clients.Clientset, ns, c.cfg.ResourceNamespace)
	for _, p := range oldPods {
		res.AffectedPods = append(res.AffectedPods, p.Name)
	}
	sort.Strings(res.AffectedPods)

	token := fmt.Sprintf("bench-%d", time.Now().UTC().UnixNano())
	patchObj := map[string]any{
		"spec": map[string]any{
			"template": map[string]any{
				"metadata": map[string]any{
					"annotations": map[string]any{
						rolloutTriggerAnnotationKey: token,
					},
				},
			},
		},
	}
	patchBytes, err := json.Marshal(patchObj)
	if err != nil {
		return res, err
	}

	patched, err := c.clients.Clientset.AppsV1().DaemonSets(ns).Patch(ctx, dsName, types.MergePatchType, patchBytes, metav1.PatchOptions{})
	if err != nil {
		res.DurationSeconds = time.Since(start).Seconds()
		return res, fmt.Errorf("patch DaemonSet %s/%s: %w", ns, dsName, err)
	}
	targetGen := patched.Generation

	waitCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	for {
		cur, getErr := c.clients.Clientset.AppsV1().DaemonSets(ns).Get(waitCtx, dsName, metav1.GetOptions{})
		if getErr == nil && cur != nil {
			st := cur.Status
			if st.ObservedGeneration >= targetGen &&
				st.DesiredNumberScheduled > 0 &&
				st.UpdatedNumberScheduled == st.DesiredNumberScheduled &&
				st.NumberReady == st.DesiredNumberScheduled &&
				st.NumberUnavailable == 0 {
				res.RolloutCompleted = true
				res.DurationSeconds = time.Since(start).Seconds()
				res.Detail = fmt.Sprintf("daemonset %s/%s rolled out %d/%d ready pods (gen %d)", ns, dsName, st.NumberReady, st.DesiredNumberScheduled, st.ObservedGeneration)
				return res, nil
			}
		}

		select {
		case <-waitCtx.Done():
			res.DurationSeconds = time.Since(start).Seconds()
			res.Detail = fmt.Sprintf("timed out after %.1fs waiting for DaemonSet %s/%s rollout", res.DurationSeconds, ns, dsName)
			return res, fmt.Errorf("daemonset rollout timeout (%s/%s): %w", ns, dsName, waitCtx.Err())
		case <-time.After(opts.PollInterval):
		}
	}
}

func (c *ClusterOps) resolveSandboxPool(ctx context.Context, poolName string) (*unstructured.Unstructured, schema.GroupVersionResource, error) {
	ns := c.cfg.ResourceNamespace
	if ns == "" {
		ns = DefaultResourceNamespace
	}
	poolName = strings.TrimSpace(poolName)

	gvrs := []schema.GroupVersionResource{SandboxPoolGVRV1Alpha2, SandboxPoolGVRV1Alpha1}
	var lastErr error
	for _, gvr := range gvrs {
		if poolName != "" {
			obj, err := c.clients.DynamicClient.Resource(gvr).Namespace(ns).Get(ctx, poolName, metav1.GetOptions{})
			if err == nil {
				return obj, gvr, nil
			}
			lastErr = err
			continue
		}
		list, err := c.clients.DynamicClient.Resource(gvr).Namespace(ns).List(ctx, metav1.ListOptions{})
		if err == nil && len(list.Items) > 0 {
			for i := range list.Items {
				if list.Items[i].GetLabels()[colocationTempLabelKey] == "true" {
					continue
				}
				return list.Items[i].DeepCopy(), gvr, nil
			}
			return list.Items[0].DeepCopy(), gvr, nil
		}
		if err != nil {
			lastErr = err
		}
	}
	if lastErr != nil {
		return nil, schema.GroupVersionResource{}, fmt.Errorf("resolve SandboxPool %q in %s: %w", poolName, ns, lastErr)
	}
	return nil, schema.GroupVersionResource{}, fmt.Errorf("no SandboxPool found in namespace %s", ns)
}

func (c *ClusterOps) listPoolFastletPods(ctx context.Context, ns, poolName string) ([]corev1.Pod, error) {
	podList, err := c.clients.Clientset.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	var matched []corev1.Pod
	for i := range podList.Items {
		p := &podList.Items[i]
		if !isFastletPod(p) {
			continue
		}
		if poolName != "" && !podBelongsToPool(p, poolName) {
			continue
		}
		matched = append(matched, *p)
	}
	return matched, nil
}

func podBelongsToPool(pod *corev1.Pod, poolName string) bool {
	if pod == nil || poolName == "" {
		return false
	}
	for _, k := range []string{"sandbox-pool", "fast-sandbox.io/pool", "sandbox.fast.io/pool", "pool"} {
		if pod.Labels[k] == poolName {
			return true
		}
	}
	for _, owner := range pod.OwnerReferences {
		if owner.Kind == "SandboxPool" && owner.Name == poolName {
			return true
		}
	}
	return strings.HasPrefix(pod.Name, poolName+"-")
}

func (c *ClusterOps) stampSandboxPoolTemplate(ctx context.Context, gvr schema.GroupVersionResource, ns string, poolObj *unstructured.Unstructured, token string) error {
	updated := poolObj.DeepCopy()

	// Stamp fastletTemplate.metadata.annotations.
	annots, _, _ := unstructured.NestedStringMap(updated.Object, "spec", "fastletTemplate", "metadata", "annotations")
	if annots == nil {
		annots = make(map[string]string)
	}
	annots[rolloutTriggerAnnotationKey] = token
	if err := unstructured.SetNestedStringMap(updated.Object, annots, "spec", "fastletTemplate", "metadata", "annotations"); err != nil {
		return err
	}

	// Also stamp an env var on the primary fastlet container so controllers hashing only spec.containers detect the rollout.
	containers, found, err := unstructured.NestedSlice(updated.Object, "spec", "fastletTemplate", "spec", "containers")
	if err == nil && found && len(containers) > 0 {
		if first, ok := containers[0].(map[string]any); ok {
			envSlice, _, _ := unstructured.NestedSlice(first, "env")
			replaced := false
			for i, item := range envSlice {
				if m, ok := item.(map[string]any); ok && m["name"] == rolloutTriggerEnvName {
					m["value"] = token
					envSlice[i] = m
					replaced = true
					break
				}
			}
			if !replaced {
				envSlice = append(envSlice, map[string]any{
					"name":  rolloutTriggerEnvName,
					"value": token,
				})
			}
			first["env"] = envSlice
			containers[0] = first
			if err := unstructured.SetNestedSlice(updated.Object, containers, "spec", "fastletTemplate", "spec", "containers"); err != nil {
				return err
			}
		}
	}

	_, err = c.clients.DynamicClient.Resource(gvr).Namespace(ns).Update(ctx, updated, metav1.UpdateOptions{})
	return err
}

func buildColocationPoolName(basePool string, suffix int64) string {
	shortSuffix := fmt.Sprintf("%x", suffix&0xfffff)
	prefix := strings.TrimSuffix(basePool, "-")
	maxPrefix := 52 - len("-coloc-") - len(shortSuffix)
	if len(prefix) > maxPrefix {
		prefix = strings.TrimSuffix(prefix[:maxPrefix], "-")
	}
	if prefix == "" {
		prefix = "bench-pool"
	}
	return fmt.Sprintf("%s-coloc-%s", prefix, shortSuffix)
}

func buildColocationPoolObject(primary *unstructured.Unstructured, name, ns, targetNode string) *unstructured.Unstructured {
	specCopy := map[string]any{}
	if rawSpec, found, _ := unstructured.NestedMap(primary.Object, "spec"); found && rawSpec != nil {
		specCopy = runtimeDeepCopyMap(rawSpec)
	}

	specCopy["capacity"] = map[string]any{
		"poolMin":   int64(1),
		"poolMax":   int64(1),
		"bufferMin": int64(0),
		"bufferMax": int64(0),
	}

	ft, _ := specCopy["fastletTemplate"].(map[string]any)
	if ft == nil {
		ft = map[string]any{}
	}
	ftSpec, _ := ft["spec"].(map[string]any)
	if ftSpec == nil {
		ftSpec = map[string]any{}
	}
	// Remove podAntiAffinity so this second Fastlet can co-locate on targetNode.
	delete(ftSpec, "affinity")
	nodeSel := map[string]any{
		"kubernetes.io/hostname": targetNode,
	}
	if existingSel, ok := ftSpec["nodeSelector"].(map[string]any); ok {
		for k, v := range existingSel {
			nodeSel[k] = v
		}
		nodeSel["kubernetes.io/hostname"] = targetNode
	}
	ftSpec["nodeSelector"] = nodeSel
	ft["spec"] = ftSpec
	specCopy["fastletTemplate"] = ft

	apiVersion := primary.GetAPIVersion()
	if apiVersion == "" {
		apiVersion = "sandbox.fast.io/v1alpha2"
	}

	return &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": apiVersion,
			"kind":       "SandboxPool",
			"metadata": map[string]any{
				"name":      name,
				"namespace": ns,
				"labels": map[string]any{
					colocationTempLabelKey: "true",
				},
			},
			"spec": specCopy,
		},
	}
}

func runtimeDeepCopyMap(in map[string]any) map[string]any {
	raw, err := json.Marshal(in)
	if err != nil {
		out := make(map[string]any, len(in))
		for k, v := range in {
			out[k] = v
		}
		return out
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return map[string]any{}
	}
	return out
}

func minDuration(a, b time.Duration) time.Duration {
	if a <= 0 {
		return b
	}
	if a < b {
		return a
	}
	return b
}
