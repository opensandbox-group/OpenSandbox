//go:build !linux

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

package resources

import "errors"

// diskUsages: statfs-based mount enumeration is only implemented for Linux,
// the platform sandboxes run on.
func diskUsages() ([]DiskUsage, error) {
	return nil, errors.New("disk usage metrics are only available on Linux")
}

// diskIO: cgroup IO accounting and /proc/diskstats are Linux sources.
func diskIO() (DiskIO, error) {
	return DiskIO{}, errors.New("disk IO metrics are only available on Linux")
}
