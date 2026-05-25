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

package telemetry

import (
	"os"
	"strings"

	"go.opentelemetry.io/otel/attribute"
)

type SharedAttrsEnvConfig struct {
	SandboxIDEnv  string
	ExtraAttrsEnv string
	SandboxAttr   string
}

func SharedAttrsFromEnv(cfg SharedAttrsEnvConfig) []attribute.KeyValue {
	attrKey := strings.TrimSpace(cfg.SandboxAttr)
	if attrKey == "" {
		attrKey = "sandbox_id"
	}

	var kvs []attribute.KeyValue
	if id := strings.TrimSpace(os.Getenv(cfg.SandboxIDEnv)); id != "" {
		kvs = append(kvs, attribute.String(attrKey, id))
	}
	return AppendAttrsFromKeyValuePairs(kvs, os.Getenv(cfg.ExtraAttrsEnv))
}

func AppendAttrsFromKeyValuePairs(kvs []attribute.KeyValue, raw string) []attribute.KeyValue {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return kvs
	}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		i := strings.IndexByte(part, '=')
		if i <= 0 || i == len(part)-1 {
			continue
		}
		key := strings.TrimSpace(part[:i])
		value := strings.TrimSpace(part[i+1:])
		if key == "" {
			continue
		}
		kvs = append(kvs, attribute.String(key, value))
	}
	return kvs
}
