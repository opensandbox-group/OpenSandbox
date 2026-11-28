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

package runtime

import (
	"errors"
	"sync/atomic"

	"github.com/alibaba/opensandbox/execd/pkg/isolation"
	"github.com/alibaba/opensandbox/execd/pkg/log"
)

const (
	CgroupProbeArg = "--cgroup-probe"
	CgroupWaitArg  = "--cgroup-wait"
)

var otherCgroupQoSRequested atomic.Bool

// InitCgroupQoS is a no-op off Linux; it validates static configuration and
// records whether cgroup v2 isolation was requested for capabilities reporting.
func InitCgroupQoS(cfg isolation.Config, _ bool, _ bool) error {
	if err := cfg.QoS.Validate(); err != nil {
		return err
	}
	otherCgroupQoSRequested.Store(cfg.QoS.Cgroup.Enabled)
	if cfg.QoS.Cgroup.Enabled {
		log.Warn("qos.cgroup: cgroup v2 QoS isolation is unsupported on this platform (Linux-only)")
	}
	return nil
}

// RunCgroupWaitHelper is Linux-only.
func RunCgroupWaitHelper(_ []string) error {
	return errors.New("cgroup wait helper is Linux-only")
}
