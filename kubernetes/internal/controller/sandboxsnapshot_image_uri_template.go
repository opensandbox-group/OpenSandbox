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
	"fmt"
	"strings"
	"text/template"
	"time"

	"github.com/distribution/reference"
	corev1 "k8s.io/api/core/v1"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
)

// DefaultSnapshotImageURITemplate preserves the original snapshot naming rule.
const DefaultSnapshotImageURITemplate = "{{.Registry}}/{{.SandboxName}}-{{.ContainerName}}:{{.SnapshotTag}}"

var defaultSnapshotImageURITemplate = template.Must(ParseSnapshotImageURITemplate(DefaultSnapshotImageURITemplate))

// snapshotImageURITemplateData is the named-field contract for operator templates.
type snapshotImageURITemplateData struct {
	Registry             string
	Namespace            string
	SandboxName          string
	SnapshotName         string
	SnapshotUID          string
	SnapshotCreationTime time.Time
	ContainerName        string
	SnapshotTag          string
	ArtifactKind         string
}

// ParseSnapshotImageURITemplate parses an operator template once at startup.
// An empty value returns nil to select the backwards-compatible default.
func ParseSnapshotImageURITemplate(value string) (*template.Template, error) {
	if value == "" {
		return nil, nil
	}
	return template.New("snapshot-image-uri").Option("missingkey=error").Funcs(template.FuncMap{
		"date":       formatSnapshotDate,
		"dateInZone": formatSnapshotDateInZone,
	}).Parse(value)
}

// The date helpers use Helm's function names and argument order. Only time.Time
// inputs are accepted, so an invalid input can never fall back to the current time.
func formatSnapshotDate(layout string, value time.Time) (string, error) {
	return formatSnapshotDateInZone(layout, value, "Local")
}

func formatSnapshotDateInZone(layout string, value time.Time, zone string) (string, error) {
	if value.IsZero() {
		return "", fmt.Errorf("snapshot creation timestamp is not set")
	}
	location, err := snapshotTimeZone(zone)
	if err != nil {
		return "", err
	}
	return value.In(location).Format(layout), nil
}

func snapshotTimeZone(zone string) (*time.Location, error) {
	switch zone {
	case "", "Local":
		return time.Local, nil
	case "UTC", "Z":
		return time.UTC, nil
	}
	if strings.HasPrefix(zone, "+") || strings.HasPrefix(zone, "-") {
		if len(zone) != 6 || zone[3] != ':' ||
			zone[1] < '0' || zone[1] > '9' || zone[2] < '0' || zone[2] > '9' ||
			zone[4] < '0' || zone[4] > '9' || zone[5] < '0' || zone[5] > '9' {
			return nil, fmt.Errorf("invalid snapshot timezone offset %q: expected +HH:MM or -HH:MM", zone)
		}
		hours := int(zone[1]-'0')*10 + int(zone[2]-'0')
		minutes := int(zone[4]-'0')*10 + int(zone[5]-'0')
		if hours > 23 || minutes > 59 {
			return nil, fmt.Errorf("invalid snapshot timezone offset %q: hours must be 00-23 and minutes 00-59", zone)
		}
		offset := hours*60*60 + minutes*60
		if zone[0] == '-' {
			offset = -offset
		}
		return time.FixedZone(zone, offset), nil
	}
	if !strings.Contains(zone, "/") {
		return nil, fmt.Errorf("invalid snapshot timezone %q: expected Local, UTC, an IANA Area/Location, or a signed HH:MM offset", zone)
	}
	location, err := time.LoadLocation(zone)
	if err != nil {
		return nil, fmt.Errorf("load snapshot timezone %q: %w", zone, err)
	}
	return location, nil
}

func (r *SandboxSnapshotReconciler) snapshotImageURI(
	snapshot *sandboxv1alpha1.SandboxSnapshot,
	bs *sandboxv1alpha1.BatchSandbox,
	containerName, artifactKind string,
) (string, error) {
	tmpl := r.SnapshotImageURITemplate
	if tmpl == nil {
		tmpl = defaultSnapshotImageURITemplate
	}
	var output strings.Builder
	if err := tmpl.Execute(&output, snapshotImageURITemplateData{
		Registry:             r.SnapshotRegistry,
		Namespace:            snapshot.Namespace,
		SandboxName:          bs.Name,
		SnapshotName:         snapshot.Name,
		SnapshotUID:          string(snapshot.UID),
		SnapshotCreationTime: snapshot.CreationTimestamp.Time.Local(),
		ContainerName:        containerName,
		SnapshotTag:          snapshotImageTag(snapshot, bs),
		ArtifactKind:         artifactKind,
	}); err != nil {
		return "", fmt.Errorf("render snapshot image URI template for %s %q: %w", artifactKind, containerName, err)
	}
	imageURI := output.String()
	ref, err := reference.ParseNormalizedNamed(imageURI)
	if err != nil {
		return "", fmt.Errorf("invalid snapshot image URI %q: %w", imageURI, err)
	}
	_, tagged := ref.(reference.Tagged)
	_, digested := ref.(reference.Digested)
	if !tagged || digested {
		return "", fmt.Errorf("snapshot image URI %q must have a tag and no digest", imageURI)
	}
	if r.SnapshotImageURITemplate == nil {
		// Preserve existing registry prefixes when no custom template is configured.
		return imageURI, nil
	}
	// The push path requires a fully qualified reference; use the same target
	// that validation and duplicate detection resolve from the template output.
	return ref.String(), nil
}

func (r *SandboxSnapshotReconciler) resolveSnapshotImages(
	snapshot *sandboxv1alpha1.SandboxSnapshot,
	bs *sandboxv1alpha1.BatchSandbox,
	sourceContainers []corev1.Container,
	includeVMState bool,
) ([]sandboxv1alpha1.ContainerSnapshot, string, error) {
	seen := make(map[string]bool)
	render := func(containerName, artifactKind string) (string, error) {
		imageURI, err := r.snapshotImageURI(snapshot, bs, containerName, artifactKind)
		if err != nil {
			return "", err
		}
		// Compare normalized references so aliases cannot hide a collision.
		ref, err := reference.ParseNormalizedNamed(imageURI)
		if err != nil {
			return "", err
		}
		if seen[ref.String()] {
			return "", fmt.Errorf("snapshot image URI template produces duplicate target %q", imageURI)
		}
		seen[ref.String()] = true
		return imageURI, nil
	}
	containers := make([]sandboxv1alpha1.ContainerSnapshot, 0, len(sourceContainers))
	for _, c := range sourceContainers {
		imageURI, err := render(c.Name, "rootfs")
		if err != nil {
			return nil, "", err
		}
		containers = append(containers, sandboxv1alpha1.ContainerSnapshot{
			ContainerName: c.Name,
			ImageURI:      imageURI,
		})
	}
	var vmStateImageURI string
	if includeVMState {
		var err error
		vmStateImageURI, err = render("vmstate", "vmstate")
		if err != nil {
			return nil, "", err
		}
	}
	return containers, vmStateImageURI, nil
}
