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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
)

func TestSnapshotImageURITemplateDateFunctions(t *testing.T) {
	// These tests are deliberately sequential because time.Local is process-wide.
	originalLocal := time.Local
	time.Local = time.FixedZone("test-local", 8*60*60)
	t.Cleanup(func() { time.Local = originalLocal })

	for _, tc := range []struct {
		name, expression, createdAt, want, wantError string
		missingTime                                  bool
	}{
		{name: "native local", expression: `.SnapshotCreationTime.Format "2006-01-02"`, want: "2026-09-14"},
		{name: "native UTC", expression: `.SnapshotCreationTime.UTC.Format "2006-01-02"`, want: "2026-09-13"},
		{name: "date defaults to local", expression: `date "2006-01-02" .SnapshotCreationTime`, want: "2026-09-14"},
		{name: "date pipeline", expression: `.SnapshotCreationTime | date "2006-01-02"`, want: "2026-09-14"},
		{name: "UTC", expression: `dateInZone "2006-01-02" .SnapshotCreationTime "UTC"`, want: "2026-09-13"},
		{name: "Z", expression: `dateInZone "2006-01-02" .SnapshotCreationTime "Z"`, want: "2026-09-13"},
		{name: "Local", expression: `dateInZone "2006-01-02" .SnapshotCreationTime "Local"`, want: "2026-09-14"},
		{name: "empty zone is local", expression: `dateInZone "2006-01-02" .SnapshotCreationTime ""`, want: "2026-09-14"},
		{name: "positive offset", expression: `dateInZone "2006-01-02" .SnapshotCreationTime "+08:00"`, want: "2026-09-14"},
		{name: "negative offset", expression: `dateInZone "2006-01-02" .SnapshotCreationTime "-05:00"`, want: "2026-09-13"},
		{name: "positive zero offset", expression: `dateInZone "2006-01-02" .SnapshotCreationTime "+00:00"`, want: "2026-09-13"},
		{name: "negative zero offset", expression: `dateInZone "2006-01-02" .SnapshotCreationTime "-00:00"`, want: "2026-09-13"},
		{name: "half-hour offset", expression: `dateInZone "2006-01-02-15-04-05" .SnapshotCreationTime "+05:30"`, want: "2026-09-14-02-00-45"},
		{name: "quarter-hour offset", expression: `dateInZone "2006-01-02-15-04-05" .SnapshotCreationTime "+05:45"`, want: "2026-09-14-02-15-45"},
		{name: "negative half-hour offset", expression: `dateInZone "2006-01-02-15-04-05" .SnapshotCreationTime "-03:30"`, want: "2026-09-13-17-00-45"},
		{name: "compact milliseconds", expression: `date "20060102-150405.000" .SnapshotCreationTime`, want: "20260914-043045.123"},
		{name: "monthly grouping", expression: `date "2006-01" .SnapshotCreationTime`, want: "2026-09"},
		{name: "year rollover", expression: `date "2006-01-02" .SnapshotCreationTime`, createdAt: "2026-12-31T20:30:00Z", want: "2027-01-01"},
		{name: "leap day", expression: `dateInZone "2006-01-02" .SnapshotCreationTime "-01:00"`, createdAt: "2024-03-01T00:30:00Z", want: "2024-02-29"},
		{name: "named zone", expression: `dateInZone "2006-01-02" .SnapshotCreationTime "Asia/Shanghai"`, want: "2026-09-14"},
		{name: "before spring transition", expression: `dateInZone "2006-01-02-15-04-05-0700" .SnapshotCreationTime "America/New_York"`, createdAt: "2026-03-08T06:59:00Z", want: "2026-03-08-01-59-00-0500"},
		{name: "after spring transition", expression: `dateInZone "2006-01-02-15-04-05-0700" .SnapshotCreationTime "America/New_York"`, createdAt: "2026-03-08T07:01:00Z", want: "2026-03-08-03-01-00-0400"},
		{name: "before fall transition", expression: `dateInZone "2006-01-02-15-04-05-0700" .SnapshotCreationTime "America/New_York"`, createdAt: "2026-11-01T05:30:00Z", want: "2026-11-01-01-30-00-0400"},
		{name: "after fall transition", expression: `dateInZone "2006-01-02-15-04-05-0700" .SnapshotCreationTime "America/New_York"`, createdAt: "2026-11-01T06:30:00Z", want: "2026-11-01-01-30-00-0500"},
		{name: "fixed offset ignores DST", expression: `dateInZone "2006-01-02-15-04-05" .SnapshotCreationTime "-05:00"`, createdAt: "2026-03-08T07:01:00Z", want: "2026-03-08-02-01-00"},
		{name: "unknown named zone", expression: `dateInZone "2006-01-02" .SnapshotCreationTime "Invalid/Zone"`, wantError: "load snapshot timezone"},
		{name: "ambiguous abbreviation", expression: `dateInZone "2006-01-02" .SnapshotCreationTime "CST"`, wantError: "invalid snapshot timezone"},
		{name: "missing timestamp", expression: `date "2006-01-02" .SnapshotCreationTime`, missingTime: true, wantError: "snapshot creation timestamp is not set"},
		{name: "wrong timestamp type", expression: `date "2006-01-02" .SnapshotName`, wantError: "expected time.Time"},
		{name: "invalid image characters", expression: `date "2006-01-02 15:04:05" .SnapshotCreationTime`, wantError: "invalid snapshot image URI"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			createdAt := tc.createdAt
			if createdAt == "" {
				createdAt = "2026-09-13T20:30:45.123Z"
			}
			created, err := time.Parse(time.RFC3339Nano, createdAt)
			require.NoError(t, err)
			if tc.missingTime {
				created = time.Time{}
			}
			owner := true
			snapshot := &sandboxv1alpha1.SandboxSnapshot{ObjectMeta: metav1.ObjectMeta{
				Name: "snapshot", CreationTimestamp: metav1.NewTime(created),
				OwnerReferences: []metav1.OwnerReference{{Kind: "BatchSandbox", Controller: &owner}},
			}}
			bs := &sandboxv1alpha1.BatchSandbox{ObjectMeta: metav1.ObjectMeta{Name: "sandbox", Generation: 3}}
			tmpl, err := ParseSnapshotImageURITemplate("{{.Registry}}/snapshots-{{" + tc.expression + "}}:{{.SnapshotTag}}")
			require.NoError(t, err)
			r := &SandboxSnapshotReconciler{SnapshotRegistry: "registry.example/team", SnapshotImageURITemplate: tmpl}
			got, err := r.snapshotImageURI(snapshot, bs, "main", "rootfs")
			if tc.wantError != "" {
				require.ErrorContains(t, err, tc.wantError)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "registry.example/team/snapshots-"+tc.want+":snap-gen3", got)
		})
	}
}

func TestSnapshotTimeZoneInvalidOffsets(t *testing.T) {
	for _, zone := range []string{
		"+", "-", "+0800", "+8:00", "+08:0", "+0a:00", "+08:aa",
		"+24:00", "-24:00", "+08:60", "-08:60", "+08:00:00",
	} {
		t.Run(zone, func(t *testing.T) {
			_, err := snapshotTimeZone(zone)
			require.ErrorContains(t, err, "invalid snapshot timezone offset")
		})
	}
}
