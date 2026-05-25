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

package controller

import (
	"time"

	"github.com/alibaba/opensandbox/execd/pkg/telemetry"
)

type filesystemMetricRecorder struct {
	start     time.Time
	operation string
	success   bool
}

func beginFilesystemMetric(operation string) *filesystemMetricRecorder {
	return &filesystemMetricRecorder{
		start:     time.Now(),
		operation: operation,
	}
}

func (r *filesystemMetricRecorder) MarkSuccess() {
	r.success = true
}

func (r *filesystemMetricRecorder) Finish(ctrl *basicController) {
	result := "failure"
	if r.success {
		result = "success"
	}
	telemetry.RecordFilesystemOperation(
		ctrl.ctx.Request.Context(),
		r.operation,
		result,
		float64(time.Since(r.start))/float64(time.Millisecond),
	)
}
