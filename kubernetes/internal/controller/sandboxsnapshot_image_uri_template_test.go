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

package controller

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	containerdref "github.com/containerd/containerd/reference"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
	snapshotcontract "github.com/alibaba/OpenSandbox/sandbox-k8s/internal/snapshot"
)

func TestSnapshotImageURITemplate(t *testing.T) {
	snapshot := &sandboxv1alpha1.SandboxSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "osb-snap-11111111222243338444555555555555", Namespace: "team", UID: "snapshot-uid"},
	}
	bs := &sandboxv1alpha1.BatchSandbox{ObjectMeta: metav1.ObjectMeta{Name: "sandbox", Generation: 3}}
	const publicTag = "snap-11111111222243338444555555555555"
	for _, tc := range []struct {
		name, value, want, wantError string
	}{
		{name: "default", want: "localhost:5000/team/sandbox-main:" + publicTag},
		{name: "named fields", value: "{{.Registry}}/{{.Namespace}}/snapshots:{{.SnapshotUID}}-{{.SandboxName}}-{{.ContainerName}}-{{.ArtifactKind}}-{{.SnapshotTag}}", want: "localhost:5000/team/team/snapshots:snapshot-uid-sandbox-main-rootfs-" + publicTag},
		{name: "snapshot name", value: "{{.Registry}}/snapshots:{{.SnapshotName}}-{{.ContainerName}}", want: "localhost:5000/team/snapshots:" + snapshot.Name + "-main"},
		{name: "shorthand repository", value: "snapshots:{{.SnapshotTag}}", want: "docker.io/library/snapshots:" + publicTag},
		{name: "shorthand namespace", value: "team/snapshots:{{.SnapshotTag}}", want: "docker.io/team/snapshots:" + publicTag},
		{name: "Docker Hub default namespace", value: "docker.io/snapshots:{{.SnapshotTag}}", want: "docker.io/library/snapshots:" + publicTag},
		{name: "Docker Hub alias", value: "index.docker.io/team/snapshots:{{.SnapshotTag}}", want: "docker.io/team/snapshots:" + publicTag},
		{name: "unknown field", value: "{{.Registrry}}/snapshots:tag", wantError: "Registrry"},
		{name: "missing tag", value: "{{.Registry}}/snapshots", wantError: "must have a tag"},
		{name: "digest", value: "{{.Registry}}/snapshots:tag@sha256:" + strings.Repeat("a", 64), wantError: "no digest"},
		{name: "invalid repository", value: "{{.Registry}}/UPPER:tag", wantError: "invalid snapshot image URI"},
		{name: "whitespace", value: " {{.Registry}}/snapshots:tag", wantError: "invalid snapshot image URI"},
		{name: "long tag", value: "{{.Registry}}/snapshots:" + strings.Repeat("a", 129), wantError: "invalid snapshot image URI"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmpl, err := ParseSnapshotImageURITemplate(tc.value)
			require.NoError(t, err)
			r := &SandboxSnapshotReconciler{SnapshotRegistry: "localhost:5000/team", SnapshotImageURITemplate: tmpl}
			got, err := r.snapshotImageURI(snapshot, bs, "main", "rootfs")
			if tc.wantError != "" {
				require.ErrorContains(t, err, tc.wantError)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
	_, err := ParseSnapshotImageURITemplate("{{.Registry}")
	require.Error(t, err)

	controller := true
	snapshot.OwnerReferences = []metav1.OwnerReference{{Kind: "BatchSandbox", Controller: &controller}}
	r := &SandboxSnapshotReconciler{SnapshotRegistry: "localhost:5000/team"}
	containers, vmstate, err := r.resolveSnapshotImages(snapshot, bs, []corev1.Container{{Name: "main"}}, true)
	require.NoError(t, err)
	assert.Equal(t, "localhost:5000/team/sandbox-main:snap-gen3", containers[0].ImageURI)
	assert.Equal(t, "localhost:5000/team/sandbox-vmstate:snap-gen3", vmstate)
}

func TestSnapshotImageURITemplateDefaultPreservesRegistryPrefix(t *testing.T) {
	controller := true
	snapshot := &sandboxv1alpha1.SandboxSnapshot{ObjectMeta: metav1.ObjectMeta{
		OwnerReferences: []metav1.OwnerReference{{Kind: "BatchSandbox", Controller: &controller}},
	}}
	bs := &sandboxv1alpha1.BatchSandbox{ObjectMeta: metav1.ObjectMeta{Name: "sandbox", Generation: 3}}
	for _, registry := range []string{"registry", "registry/team", "localhost:5000/team", "registry.example/team"} {
		for _, configured := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/configured=%t", registry, configured), func(t *testing.T) {
				r := &SandboxSnapshotReconciler{SnapshotRegistry: registry}
				if configured {
					var err error
					r.SnapshotImageURITemplate, err = ParseSnapshotImageURITemplate("")
					require.NoError(t, err)
				}
				containers, vmstate, err := r.resolveSnapshotImages(snapshot, bs, []corev1.Container{{Name: "main"}}, true)
				require.NoError(t, err)
				assert.Equal(t, registry+"/sandbox-main:snap-gen3", containers[0].ImageURI)
				assert.Equal(t, registry+"/sandbox-vmstate:snap-gen3", vmstate)
			})
		}
	}
}

func TestSnapshotImageURITemplateCreationTime(t *testing.T) {
	tmpl, err := ParseSnapshotImageURITemplate(`{{.Registry}}/library/snapshots-{{.SnapshotCreationTime.UTC.Format "2006-01-02"}}:{{.SandboxName}}-{{.ContainerName}}-{{.SnapshotTag}}`)
	require.NoError(t, err)
	r := &SandboxSnapshotReconciler{SnapshotRegistry: "registry.example/team", SnapshotImageURITemplate: tmpl}
	bs := &sandboxv1alpha1.BatchSandbox{ObjectMeta: metav1.ObjectMeta{Name: "sandbox", Generation: 3}}
	for _, tc := range []struct {
		name, createdAt, wantDate string
	}{
		{name: "UTC", createdAt: "2026-09-13T00:00:00Z", wantDate: "2026-09-13"},
		{name: "previous UTC day", createdAt: "2026-09-14T00:30:00+08:00", wantDate: "2026-09-13"},
		{name: "next UTC day", createdAt: "2026-09-12T23:30:00-07:00", wantDate: "2026-09-13"},
		{name: "different snapshot date", createdAt: "2026-09-14T00:00:00Z", wantDate: "2026-09-14"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			createdAt, err := time.Parse(time.RFC3339, tc.createdAt)
			require.NoError(t, err)
			controller := true
			snapshot := &sandboxv1alpha1.SandboxSnapshot{ObjectMeta: metav1.ObjectMeta{
				Name:              "snapshot",
				CreationTimestamp: metav1.NewTime(createdAt),
				OwnerReferences:   []metav1.OwnerReference{{Kind: "BatchSandbox", Controller: &controller}},
			}}
			got, err := r.snapshotImageURI(snapshot, bs, "main", "rootfs")
			require.NoError(t, err)
			assert.Equal(t, "registry.example/team/library/snapshots-"+tc.wantDate+":sandbox-main-snap-gen3", got)
		})
	}
}

func TestSandboxSnapshotHandlePending_ImageURITemplate(t *testing.T) {
	for _, tc := range []struct {
		name, value, wantError, imagePrefix string
		qemu                                bool
	}{
		{name: "rootfs", value: "{{.Registry}}/snapshots:{{.SnapshotUID}}-{{.ContainerName}}-{{.ArtifactKind}}-{{.SnapshotTag}}"},
		{name: "qemu", value: "{{.Registry}}/snapshots:{{.SnapshotUID}}-{{.ContainerName}}-{{.ArtifactKind}}-{{.SnapshotTag}}", qemu: true},
		{name: "shorthand rootfs", value: "snapshots:{{.SnapshotUID}}-{{.ContainerName}}-{{.ArtifactKind}}-{{.SnapshotTag}}", imagePrefix: "docker.io/library/snapshots:snapshot-uid-"},
		{name: "shorthand qemu", value: "snapshots:{{.SnapshotUID}}-{{.ContainerName}}-{{.ArtifactKind}}-{{.SnapshotTag}}", imagePrefix: "docker.io/library/snapshots:snapshot-uid-", qemu: true},
		{name: "shorthand namespace rootfs", value: "team/snapshots:{{.SnapshotUID}}-{{.ContainerName}}-{{.ArtifactKind}}-{{.SnapshotTag}}", imagePrefix: "docker.io/team/snapshots:snapshot-uid-"},
		{name: "shorthand namespace qemu", value: "team/snapshots:{{.SnapshotUID}}-{{.ContainerName}}-{{.ArtifactKind}}-{{.SnapshotTag}}", imagePrefix: "docker.io/team/snapshots:snapshot-uid-", qemu: true},
		{name: "dated rootfs", value: `{{.Registry}}/library/snapshots-{{dateInZone "2006-01-02" .SnapshotCreationTime "UTC"}}:{{.SnapshotUID}}-{{.ContainerName}}-{{.ArtifactKind}}-{{.SnapshotTag}}`, imagePrefix: "registry.example/team/library/snapshots-2026-09-13:snapshot-uid-"},
		{name: "dated qemu", value: `{{.Registry}}/library/snapshots-{{dateInZone "2006-01-02" .SnapshotCreationTime "UTC"}}:{{.SnapshotUID}}-{{.ContainerName}}-{{.ArtifactKind}}-{{.SnapshotTag}}`, imagePrefix: "registry.example/team/library/snapshots-2026-09-13:snapshot-uid-", qemu: true},
		{name: "offset qemu", value: `{{.Registry}}/library/snapshots-{{dateInZone "2006-01-02" .SnapshotCreationTime "+08:00"}}:{{.SnapshotUID}}-{{.ContainerName}}-{{.ArtifactKind}}-{{.SnapshotTag}}`, imagePrefix: "registry.example/team/library/snapshots-2026-09-14:snapshot-uid-", qemu: true},
		{name: "unknown timezone", value: `{{.Registry}}/snapshots-{{dateInZone "2006-01-02" .SnapshotCreationTime "Invalid/Zone"}}:{{.SnapshotTag}}`, wantError: "load snapshot timezone"},
		{name: "invalid offset", value: `{{.Registry}}/snapshots-{{dateInZone "2006-01-02" .SnapshotCreationTime "+08:60"}}:{{.SnapshotTag}}`, wantError: "invalid snapshot timezone offset"},
		{name: "invalid timestamp image", value: `{{.Registry}}/snapshots-{{date "2006-01-02 15:04:05" .SnapshotCreationTime}}:{{.SnapshotTag}}`, wantError: "invalid snapshot image URI"},
		{name: "unknown field", value: "{{.Unknown}}", wantError: "Unknown"},
		{name: "invalid image", value: "registry.example/no-tag", wantError: "must have a tag"},
		{name: "container collision", value: "{{.Registry}}/snapshots:{{.SnapshotTag}}", wantError: "duplicate target"},
		{name: "qemu collision", value: "{{.Registry}}/snapshots:{{if eq .ContainerName \"sidecar\"}}sidecar{{else}}main{{end}}", qemu: true, wantError: "duplicate target"},
		{name: "qemu invalid image", value: "{{.Registry}}/snapshots{{if eq .ArtifactKind \"rootfs\"}}:{{.ContainerName}}{{end}}", qemu: true, wantError: "must have a tag"},
		{name: "normalized collision", value: "{{if eq .ContainerName \"main\"}}busybox:tag{{else}}docker.io/library/busybox:tag{{end}}", wantError: "duplicate target"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			controller := true
			bs := &sandboxv1alpha1.BatchSandbox{ObjectMeta: metav1.ObjectMeta{Name: "sandbox", Namespace: "team", Generation: 3}}
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "source", Namespace: "team", UID: "pod-uid", Labels: map[string]string{labelBatchSandboxNameKey: bs.Name}},
				Spec:       corev1.PodSpec{NodeName: "node", Containers: []corev1.Container{{Name: "main"}, {Name: "sidecar"}}},
				Status:     corev1.PodStatus{Phase: corev1.PodRunning},
			}
			if tc.qemu {
				pod.Annotations = map[string]string{
					snapshotcontract.AnnotationCheckpointProvider: snapshotcontract.ProviderQEMU,
					snapshotcontract.AnnotationQEMUContainer:      "main",
					"sandbox.opensandbox.io/qemu-qmp-socket":      "/run/qemu/qmp.sock",
					"sandbox.opensandbox.io/qemu-launch-manifest": "/run/qemu/launch.json",
				}
			}
			snapshot := &sandboxv1alpha1.SandboxSnapshot{
				ObjectMeta: metav1.ObjectMeta{Name: "snapshot", Namespace: "team", UID: "snapshot-uid", OwnerReferences: []metav1.OwnerReference{{Kind: "BatchSandbox", Controller: &controller}}},
				Spec:       sandboxv1alpha1.SandboxSnapshotSpec{SandboxName: bs.Name},
				Status:     sandboxv1alpha1.SandboxSnapshotStatus{Phase: sandboxv1alpha1.SandboxSnapshotPhasePending},
			}
			snapshot.CreationTimestamp = metav1.NewTime(time.Date(2026, 9, 14, 0, 30, 0, 0, time.FixedZone("UTC+8", 8*60*60)))
			r := newTestSnapshotReconciler(bs, pod, snapshot)
			r.SnapshotRegistry = "registry.example/team"
			var err error
			r.SnapshotImageURITemplate, err = ParseSnapshotImageURITemplate(tc.value)
			require.NoError(t, err)
			_, err = r.handlePending(context.Background(), snapshot)
			require.NoError(t, err)
			updated := &sandboxv1alpha1.SandboxSnapshot{}
			require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(snapshot), updated))
			jobs := &batchv1.JobList{}
			require.NoError(t, r.List(context.Background(), jobs))
			if tc.wantError != "" {
				assert.Equal(t, sandboxv1alpha1.SandboxSnapshotPhaseFailed, updated.Status.Phase)
				require.Len(t, updated.Status.Conditions, 1)
				assert.Equal(t, "InvalidSnapshotImage", updated.Status.Conditions[0].Reason)
				assert.Contains(t, updated.Status.Conditions[0].Message, tc.wantError)
				assert.Empty(t, jobs.Items)
				assert.Empty(t, updated.Status.Containers)
				return
			}
			require.Len(t, jobs.Items, 1)
			assert.Equal(t, sandboxv1alpha1.SandboxSnapshotPhaseCommitting, updated.Status.Phase)
			require.Len(t, updated.Status.Containers, 2)
			imagePrefix := tc.imagePrefix
			if imagePrefix == "" {
				imagePrefix = "registry.example/team/snapshots:snapshot-uid-"
			}
			mainURI := imagePrefix + "main-rootfs-snap-gen3"
			sidecarURI := imagePrefix + "sidecar-rootfs-snap-gen3"
			assert.Equal(t, mainURI, updated.Status.Containers[0].ImageURI)
			assert.Equal(t, sidecarURI, updated.Status.Containers[1].ImageURI)
			for _, container := range updated.Status.Containers {
				// The push path uses containerd's parser, which requires the host
				// and repository to already be fully qualified.
				ref, err := containerdref.Parse(container.ImageURI)
				require.NoError(t, err)
				assert.Equal(t, strings.SplitN(imagePrefix, "/", 2)[0], ref.Hostname())
			}
			args := jobs.Items[0].Spec.Template.Spec.Containers[0].Args
			if tc.qemu {
				require.Equal(t, []string{"snapshot", "--request-base64"}, args[:2])
				data, err := base64.StdEncoding.DecodeString(args[2])
				require.NoError(t, err)
				var request snapshotcontract.Request
				require.NoError(t, json.Unmarshal(data, &request))
				assert.Equal(t, imagePrefix+"vmstate-vmstate-snap-gen3", request.VMStateImageURI)
				require.Len(t, request.Containers, 2)
				assert.Equal(t, mainURI, request.Containers[0].ImageURI)
				assert.Equal(t, sidecarURI, request.Containers[1].ImageURI)
			} else {
				assert.Equal(t, []string{"source", "team", "main:" + mainURI, "sidecar:" + sidecarURI}, args)
			}

			// Retry after Job creation but before the Committing status update.
			updated.Status.Phase = sandboxv1alpha1.SandboxSnapshotPhasePending
			require.NoError(t, r.Status().Update(context.Background(), updated))
			r.SnapshotImageURITemplate, err = ParseSnapshotImageURITemplate("{{.Registry}}/changed:{{.ContainerName}}")
			require.NoError(t, err)
			_, err = r.handlePending(context.Background(), updated)
			require.NoError(t, err)
			retried := &sandboxv1alpha1.SandboxSnapshot{}
			require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(snapshot), retried))
			assert.Equal(t, updated.Status.Containers, retried.Status.Containers)
			retriedJob := &batchv1.Job{}
			require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "team", Name: "snapshot-commit"}, retriedJob))
			assert.Equal(t, args, retriedJob.Spec.Template.Spec.Containers[0].Args)
		})
	}
}

func TestSandboxSnapshotHandlePending_RetriesStatusUpdateErrors(t *testing.T) {
	for _, tc := range []struct {
		name        string
		existingJob bool
	}{
		{name: "invalid image URI"},
		{name: "existing commit Job", existingJob: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bs := &sandboxv1alpha1.BatchSandbox{ObjectMeta: metav1.ObjectMeta{Name: "sandbox", Namespace: "team"}}
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "source", Namespace: "team",
					Labels: map[string]string{labelBatchSandboxNameKey: bs.Name},
				},
				Spec:   corev1.PodSpec{NodeName: "node", Containers: []corev1.Container{{Name: "main"}}},
				Status: corev1.PodStatus{Phase: corev1.PodRunning},
			}
			snapshot := &sandboxv1alpha1.SandboxSnapshot{
				ObjectMeta: metav1.ObjectMeta{Name: "snapshot", Namespace: "team"},
				Spec:       sandboxv1alpha1.SandboxSnapshotSpec{SandboxName: bs.Name},
				Status:     sandboxv1alpha1.SandboxSnapshotStatus{Phase: sandboxv1alpha1.SandboxSnapshotPhasePending},
			}
			objects := []client.Object{bs, pod, snapshot}
			if tc.existingJob {
				objects = append(objects, &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "snapshot-commit", Namespace: "team"}})
			}
			r := newTestSnapshotReconciler()
			r.SnapshotRegistry = "registry.example/team"
			var err error
			r.SnapshotImageURITemplate, err = ParseSnapshotImageURITemplate("registry.example/no-tag")
			require.NoError(t, err)
			statusErr := errors.New("status update unavailable")
			failStatusUpdate := true
			r.Client = fake.NewClientBuilder().WithScheme(r.Scheme).
				WithStatusSubresource(&sandboxv1alpha1.SandboxSnapshot{}).
				WithObjects(objects...).
				WithInterceptorFuncs(interceptor.Funcs{
					SubResourceUpdate: func(ctx context.Context, c client.Client, name string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
						if failStatusUpdate && name == "status" {
							return statusErr
						}
						return c.SubResource(name).Update(ctx, obj, opts...)
					},
				}).Build()
			ctx := context.Background()
			_, err = r.handlePending(ctx, snapshot)
			require.ErrorIs(t, err, statusErr)

			failStatusUpdate = false
			_, err = r.handlePending(ctx, snapshot)
			require.NoError(t, err)
			updated := &sandboxv1alpha1.SandboxSnapshot{}
			require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(snapshot), updated))
			jobs := &batchv1.JobList{}
			require.NoError(t, r.List(ctx, jobs))
			if tc.existingJob {
				assert.Equal(t, sandboxv1alpha1.SandboxSnapshotPhaseCommitting, updated.Status.Phase)
				assert.Len(t, jobs.Items, 1)
			} else {
				assert.Equal(t, sandboxv1alpha1.SandboxSnapshotPhaseFailed, updated.Status.Phase)
				assert.Empty(t, jobs.Items)
			}
		})
	}
}
