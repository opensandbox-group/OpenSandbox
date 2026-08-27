// Copyright 2026 Alibaba Group Holding Ltd.
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

// Package objectlayout defines the backend-independent names used by a log
// stream's data objects and finalization markers.
package objectlayout

import (
	"path"
	"strconv"
)

const markerInfix = ".finalized."

func FamilyPrefix(prefix, cluster, namespace, sandboxID, podUID string) string {
	return path.Join(prefix, cluster, namespace, sandboxID, podUID)
}

func GenerationName(container string, generation uint64) string {
	if generation == 0 {
		return container + ".log"
	}
	return container + "." + strconv.FormatUint(generation, 10) + ".log"
}

func DataKey(familyPrefix, container string, generation uint64) string {
	return path.Join(familyPrefix, GenerationName(container, generation))
}

func MarkerPrefix(familyPrefix, container string) string {
	return path.Join(familyPrefix, container) + markerInfix
}

func MarkerName(container string, revision uint64) string {
	return container + markerInfix + strconv.FormatUint(revision, 10) + ".json"
}

func MarkerKey(familyPrefix, container string, revision uint64) string {
	return path.Join(familyPrefix, MarkerName(container, revision))
}

func StreamRef(podUID, container string) string {
	return path.Join("container-logs", podUID, container)
}
