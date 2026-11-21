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

package controller

import (
	"context"
	_ "embed"
	"encoding/hex"
	"fmt"

	"path"
	"strings"

	api "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	workspacePauseAnnotation = "sandbox.opensandbox.io/workspace-pause"
	storeMount               = "/mnt/opensandbox-checkpoints"
	workspaceMount           = "/workspace"
	restoreName              = "opensandbox-workspace-restore"
)

//go:embed workspace_archive.py
var workspaceScript string

// limitedOutput drains the exec stream while retaining only a short diagnostic.
type limitedOutput struct {
	data  []byte
	limit int
}

func (o *limitedOutput) Write(p []byte) (int, error) {
	n := min(len(p), o.limit-len(o.data))
	o.data = append(o.data, p[:n]...)
	return len(p), nil
}

type workspaceExport func(context.Context, *corev1.Pod, string, string) (string, error)

func newWorkspaceExport(config *rest.Config) (workspaceExport, error) {
	cs, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context, pod *corev1.Pod, container, archive string) (string, error) {
		req := cs.CoreV1().RESTClient().Post().Resource("pods").Name(pod.Name).Namespace(pod.Namespace).SubResource("exec").
			VersionedParams(&corev1.PodExecOptions{Container: container, Command: []string{"python3", "-I", "-c", workspaceScript, "save", workspaceMount, path.Join(storeMount, archive)}, Stdout: true, Stderr: true}, scheme.ParameterCodec)
		executor, err := remotecommand.NewSPDYExecutor(config, "POST", req.URL())
		if err != nil {
			return "", err
		}
		out := limitedOutput{limit: 66}
		diagnostics := limitedOutput{limit: 1024}
		if err := executor.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: &out, Stderr: &diagnostics}); err != nil {
			return "", fmt.Errorf("%w (stderr %q)", err, diagnostics.data)
		}
		return strings.TrimSpace(string(out.data)), nil
	}, nil
}

func validWorkspaceDigest(value string) bool {
	_, err := hex.DecodeString(value)
	return len(value) == 64 && err == nil
}

func workspaceTarget(spec *corev1.PodSpec, cfg api.WorkspacePauseSpec) (*corev1.Container, string, error) {
	var claim string
	for _, v := range spec.Volumes {
		if v.Name == cfg.VolumeName && v.PersistentVolumeClaim != nil && !v.PersistentVolumeClaim.ReadOnly {
			claim = v.PersistentVolumeClaim.ClaimName
		}
	}
	if claim == "" || cfg.VolumeName == restoreName {
		return nil, "", fmt.Errorf("workspace pause requires a dedicated writable checkpoint PVC")
	}
	for i := range spec.Containers {
		c := &spec.Containers[i]
		if c.Name != cfg.ContainerName {
			continue
		}
		mounted := false
		for _, m := range c.VolumeMounts {
			if m.Name == cfg.VolumeName && m.MountPath == storeMount && !m.ReadOnly && m.SubPath == "" && m.SubPathExpr == "" {
				mounted = true
			}
			if strings.HasPrefix(m.MountPath, workspaceMount+"/") || (m.MountPath == workspaceMount && (m.Name != restoreName || m.SubPath != "data")) {
				return nil, "", fmt.Errorf("workspace pause requires a plain /workspace without nested mounts")
			}
		}
		if mounted {
			return c, claim, nil
		}
	}
	return nil, "", fmt.Errorf("mount checkpoint volume %q at %s in container %q", cfg.VolumeName, storeMount, cfg.ContainerName)
}

// prepareWorkspaceRestore is also run before export to catch template collisions while the source still exists.
func prepareWorkspaceRestore(template *corev1.PodTemplateSpec, w *api.WorkspaceCheckpoint) error {
	if template == nil || w == nil || !validArchiveName(w.Archive) {
		return fmt.Errorf("invalid workspace restore metadata")
	}
	app, claim, err := workspaceTarget(&template.Spec, w.WorkspacePauseSpec)
	if err != nil {
		return err
	}
	if claim != w.ClaimName {
		return fmt.Errorf("checkpoint PVC changed since pause")
	}
	hasVolume := false
	for _, v := range template.Spec.Volumes {
		if v.Name == restoreName {
			if v.EmptyDir == nil {
				return fmt.Errorf("reserved workspace restore volume already exists")
			}
			hasVolume = true
		}
	}
	init := corev1.Container{
		Name:            restoreName,
		Image:           app.Image,
		ImagePullPolicy: app.ImagePullPolicy,
		SecurityContext: app.SecurityContext.DeepCopy(),
		Resources:       *app.Resources.DeepCopy(),
		Command:         []string{"python3", "-I", "-c", workspaceScript, "restore", workspaceMount, path.Join(storeMount, w.Archive), w.SHA256},
		VolumeMounts:    []corev1.VolumeMount{{Name: w.VolumeName, MountPath: storeMount, ReadOnly: true}, {Name: restoreName, MountPath: workspaceMount}},
	}
	hasMount := false
	for _, m := range app.VolumeMounts {
		if m.Name == restoreName && m.MountPath == workspaceMount && m.SubPath == "data" {
			hasMount = true
		}
	}
	for i, c := range template.Spec.InitContainers {
		if c.Name != restoreName {
			continue
		}
		if !hasVolume || !hasMount || len(c.Command) != len(init.Command) || c.Command[4] != "restore" || c.Command[5] != workspaceMount {
			return fmt.Errorf("reserved workspace restore init container already exists")
		}
		template.Spec.InitContainers[i] = init
		return nil
	}
	if hasVolume || hasMount {
		return fmt.Errorf("reserved workspace restore volume already exists")
	}
	for _, c := range template.Spec.Containers {
		if c.Name == restoreName {
			return fmt.Errorf("reserved workspace restore container already exists")
		}
	}
	template.Spec.Volumes = append(template.Spec.Volumes, corev1.Volume{Name: restoreName, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}})
	app.VolumeMounts = append(app.VolumeMounts, corev1.VolumeMount{Name: restoreName, MountPath: workspaceMount, SubPath: "data"})
	template.Spec.InitContainers = append(template.Spec.InitContainers, init)
	return nil
}

func validArchiveName(name string) bool {
	return len(name) == 40 && strings.HasSuffix(name, ".tar") && strings.Trim(name[:36], "0123456789abcdef-") == ""
}

func (r *SandboxSnapshotReconciler) captureWorkspace(ctx context.Context, s *api.SandboxSnapshot, bs *api.BatchSandbox) error {
	if bs.UID == "" || !metav1.IsControlledBy(s, bs) || s.Name != internalPauseSnapshotName(bs.Name) || bs.Spec.Pause == nil || !*bs.Spec.Pause || bs.Status.Phase != api.BatchSandboxPhasePausing || bs.Spec.PoolRef != "" || bs.Spec.Template == nil {
		return fmt.Errorf("workspace capture requires an active pause on a dedicated BatchSandbox Pod")
	}
	// Read the controller-owned Pod directly, independent of user-supplied allocation annotations.
	pod := &corev1.Pod{}
	if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: bs.Namespace, Name: fmt.Sprintf("%s-%d", bs.Name, batchSandboxFirstPodIndex)}, pod); err != nil {
		return err
	}
	if !metav1.IsControlledBy(pod, bs) || pod.DeletionTimestamp != nil || pod.Status.Phase != corev1.PodRunning {
		return fmt.Errorf("source workspace Pod is unavailable")
	}
	cfg := *bs.Spec.WorkspacePause
	_, claim, err := workspaceTarget(&pod.Spec, cfg)
	if err != nil {
		return err
	}
	w := &api.WorkspaceCheckpoint{WorkspacePauseSpec: cfg, ClaimName: claim, Archive: string(uuid.NewUUID()) + ".tar", SourcePodUID: pod.UID}
	if err := prepareWorkspaceRestore(bs.Spec.Template.DeepCopy(), w); err != nil {
		return err
	}
	exportCtx, cancel := context.WithTimeout(ctx, r.getCommitJobTimeout())
	defer cancel()
	digest, err := r.exportWorkspace(exportCtx, pod, cfg.ContainerName, w.Archive)
	if err != nil {
		return fmt.Errorf("workspace export: %w", err)
	}
	if !validWorkspaceDigest(digest) {
		return fmt.Errorf("workspace export returned an invalid digest")
	}
	w.SHA256 = digest
	s.Status.SourcePodName = pod.Name
	s.Status.Workspace = w
	s.Status.Format = api.SandboxSnapshotFormatWorkspaceV1
	s.Status.Phase = api.SandboxSnapshotPhaseSucceed
	applySnapshotPhaseConditions(&s.Status, "", "")
	return r.Status().Update(ctx, s)
}
