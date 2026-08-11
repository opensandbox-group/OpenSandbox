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

//go:build linux

package resourcecheck

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/alibaba/opensandbox/nodeagent/pkg/config"
	"golang.org/x/sys/unix"
)

func validateHost(cfg config.Config) error {
	var errs []error
	var limit unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &limit); err != nil {
		errs = append(errs, fmt.Errorf("read file-descriptor limit: %w", err))
	} else if limit.Cur < 1024 {
		errs = append(errs, fmt.Errorf("file-descriptor soft limit %d is below 1024", limit.Cur))
	}
	for path, minimum := range map[string]uint64{
		"/proc/sys/fs/inotify/max_user_instances": 16,
		"/proc/sys/fs/inotify/max_user_watches":   1024,
	} {
		value, err := readUint(path)
		if err != nil {
			errs = append(errs, err)
		} else if value < minimum {
			errs = append(errs, fmt.Errorf("%s=%d is below required reserve %d", path, value, minimum))
		}
	}
	if err := checkDiskReserve(cfg.StateDir, reserveFor(cfg.StateMaxBytes)); err != nil {
		errs = append(errs, fmt.Errorf("state disk reserve: %w", err))
	}
	if cfg.Sink == config.SinkFile && cfg.FilePath != "" {
		if err := checkDiskReserve(cfg.FilePath, reserveFor(cfg.FileMaxTotalBytes)); err != nil {
			errs = append(errs, fmt.Errorf("durable-file disk reserve: %w", err))
		}
	}
	if memoryLimit, limited, err := cgroupMemoryLimit(); err != nil {
		errs = append(errs, err)
	} else if limited && cfg.MemoryBudgetBytes > int64(memoryLimit*3/4) {
		errs = append(errs, fmt.Errorf("queue memory budget %d exceeds 75%% of cgroup memory limit %d", cfg.MemoryBudgetBytes, memoryLimit))
	}
	return errors.Join(errs...)
}

func readUint(path string) (uint64, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", path, err)
	}
	value, err := strconv.ParseUint(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", path, err)
	}
	return value, nil
}

func checkDiskReserve(path string, reserve uint64) error {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return err
	}
	available := stat.Bavail * uint64(stat.Bsize)
	if available < reserve {
		return fmt.Errorf("available bytes %d are below reserve %d", available, reserve)
	}
	return nil
}

func reserveFor(limit int64) uint64 {
	reserve := uint64(limit / 20)
	if reserve < 16<<20 {
		reserve = 16 << 20
	}
	if reserve > 256<<20 {
		reserve = 256 << 20
	}
	return reserve
}

func cgroupMemoryLimit() (uint64, bool, error) {
	return cgroupMemoryLimitWithReadFile(os.ReadFile)
}

func cgroupMemoryLimitWithReadFile(readFile func(string) ([]byte, error)) (uint64, bool, error) {
	const procCgroupPath = "/proc/self/cgroup"
	rawCgroup, err := readFile(procCgroupPath)
	if err != nil {
		return 0, false, fmt.Errorf("read %s: %w", procCgroupPath, err)
	}
	type candidate struct {
		path  string
		exact bool
	}
	var paths []candidate
	for _, line := range strings.Split(strings.TrimSpace(string(rawCgroup)), "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			continue
		}
		relative := strings.TrimPrefix(filepath.Clean("/"+parts[2]), "/")
		if parts[0] == "0" && parts[1] == "" {
			paths = append(paths, candidate{path: filepath.Join("/sys/fs/cgroup", relative, "memory.max"), exact: true})
		}
		for _, controller := range strings.Split(parts[1], ",") {
			if controller == "memory" {
				paths = append(paths, candidate{path: filepath.Join("/sys/fs/cgroup/memory", relative, "memory.limit_in_bytes"), exact: true})
			}
		}
	}
	paths = append(paths,
		candidate{path: "/sys/fs/cgroup/memory.max"},
		candidate{path: "/sys/fs/cgroup/memory/memory.limit_in_bytes"},
	)
	seen := make(map[string]struct{}, len(paths))
	var errs []error
	for _, item := range paths {
		path := item.path
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		raw, err := readFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			wrapped := fmt.Errorf("read %s: %w", path, err)
			if item.exact {
				return 0, false, wrapped
			}
			errs = append(errs, wrapped)
			continue
		}
		value := strings.TrimSpace(string(raw))
		if value == "max" {
			return 0, false, nil
		}
		limit, err := strconv.ParseUint(value, 10, 64)
		if err != nil {
			wrapped := fmt.Errorf("parse %s: %w", path, err)
			if item.exact {
				return 0, false, wrapped
			}
			errs = append(errs, wrapped)
			continue
		}
		if limit >= 1<<60 {
			return 0, false, nil
		}
		return limit, true, nil
	}
	return 0, false, errors.Join(errs...)
}
