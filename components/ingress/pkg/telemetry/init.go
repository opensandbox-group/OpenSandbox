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

	inttelemetry "github.com/alibaba/opensandbox/internal/telemetry"
	"github.com/alibaba/opensandbox/internal/version"
)

const serviceName = "opensandbox-ingress"

func Init(ctx context.Context) (shutdown func(context.Context) error, err error) {
	return inttelemetry.Init(ctx, inttelemetry.Config{
		ServiceName:     serviceName + "-" + version.Version,
		RegisterMetrics: registerIngressMetrics,
	})
}
