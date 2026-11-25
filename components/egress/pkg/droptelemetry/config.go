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

package droptelemetry

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	EnvMode  = "OPENSANDBOX_EGRESS_DROP_TELEMETRY"
	EnvGroup = "OPENSANDBOX_EGRESS_DROP_LOG_GROUP"
	EnvRate  = "OPENSANDBOX_EGRESS_DROP_LOG_RATE"
	EnvBurst = "OPENSANDBOX_EGRESS_DROP_LOG_BURST"
	EnvPoll  = "OPENSANDBOX_EGRESS_DROP_COUNTER_POLL"
)

type Mode string

const (
	Off     Mode = "off"
	Counter Mode = "counter"
	Sample  Mode = "sample"
)

type Config struct {
	Mode  Mode
	Group uint16
	Rate  string
	Burst uint32
	Poll  time.Duration
}

func DefaultConfig() Config {
	return Config{Mode: Off, Group: 3085, Rate: "20/second", Burst: 40, Poll: 15 * time.Second}
}

var ratePattern = regexp.MustCompile(`^[1-9][0-9]*/(second|minute|hour|day)$`)

func Parse(getenv func(string) string) (Config, error) {
	c := DefaultConfig()
	if v := strings.TrimSpace(getenv(EnvMode)); v != "" {
		c.Mode = Mode(v)
	}
	if c.Mode != Off && c.Mode != Counter && c.Mode != Sample {
		return c, fmt.Errorf("%s must be off, counter or sample", EnvMode)
	}
	if c.Mode == Off {
		return c, nil
	}
	if v := getenv(EnvGroup); v != "" {
		n, e := strconv.ParseUint(v, 10, 16)
		if e != nil {
			return c, fmt.Errorf("invalid %s: %w", EnvGroup, e)
		}
		c.Group = uint16(n)
	}
	if v := getenv(EnvRate); v != "" {
		if !ratePattern.MatchString(v) {
			return c, fmt.Errorf("invalid %s", EnvRate)
		}
		n, e := strconv.ParseUint(strings.Split(v, "/")[0], 10, 32)
		if e != nil || n == 0 {
			return c, fmt.Errorf("invalid %s", EnvRate)
		}
		c.Rate = v
	}
	if v := getenv(EnvBurst); v != "" {
		n, e := strconv.ParseUint(v, 10, 32)
		if e != nil || n == 0 {
			return c, fmt.Errorf("invalid %s", EnvBurst)
		}
		c.Burst = uint32(n)
	}
	if v := getenv(EnvPoll); v != "" {
		d, e := time.ParseDuration(v)
		if e != nil || d <= 0 {
			return c, fmt.Errorf("invalid %s", EnvPoll)
		}
		c.Poll = d
	}
	return c, nil
}
