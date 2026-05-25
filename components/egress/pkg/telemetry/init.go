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
	"context"
	"os"
	"strings"

	"go.opentelemetry.io/otel/attribute"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
	inttelemetry "github.com/alibaba/opensandbox/internal/telemetry"
	"github.com/alibaba/opensandbox/internal/version"
)

const serviceName = "opensandbox-egress"

func Init(ctx context.Context) (shutdown func(context.Context) error, err error) {
	var attrs []attribute.KeyValue
	if id := strings.TrimSpace(os.Getenv(constants.EnvSandboxID)); id != "" {
		attrs = append(attrs, attribute.String("sandbox_id", id))
	}
	return inttelemetry.Init(ctx, inttelemetry.Config{
		ServiceName:        serviceName + "-" + version.Version,
		ResourceAttributes: attrs,
		RegisterMetrics:    registerEgressMetrics,
	})
}
