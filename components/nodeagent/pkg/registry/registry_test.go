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

package registry

import (
	"testing"

	"github.com/alibaba/opensandbox/nodeagent/pkg/api"
	"github.com/alibaba/opensandbox/nodeagent/pkg/config"
)

func TestRegisteredSinkProvidesTargetIdentity(t *testing.T) {
	const name = "test-sink-target"
	RegisterSink(
		name,
		func(cfg config.Config) (string, error) { return "target:" + cfg.ClusterID, nil },
		func(SinkDependencies) (api.Sink, error) { return nil, nil },
	)

	got, err := TargetID(name, config.Config{ClusterID: "prod-a"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "target:prod-a" {
		t.Fatalf("TargetID() = %q", got)
	}
}

func TestRegisterSourceRejectsSlashInName(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("RegisterSource() accepted a name that cannot own a stream namespace")
		}
	}()
	RegisterSource("invalid/source", func(SourceDependencies) (api.Source, error) { return nil, nil })
}
