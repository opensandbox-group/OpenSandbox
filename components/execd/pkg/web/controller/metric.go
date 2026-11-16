// Copyright 2025 The OpenSandbox Authors
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
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/alibaba/opensandbox/execd/pkg/log"
	"github.com/alibaba/opensandbox/execd/pkg/resources"
	"github.com/alibaba/opensandbox/execd/pkg/web/model"
)

type MetricController struct {
	*basicController
}

func NewMetricController(ctx *gin.Context) *MetricController {
	return &MetricController{basicController: newBasicController(ctx)}
}

func (c *MetricController) GetMetrics() {
	metrics, err := c.readMetrics()
	if err != nil {
		c.RespondError(
			http.StatusInternalServerError,
			model.ErrorCodeRuntimeError,
			fmt.Sprintf("error reading runtime metrics. %v", err),
		)
		return
	}

	c.RespondSuccess(metrics)
}

func (c *MetricController) WatchMetrics() {
	c.setupSSEResponse()

	// readMetrics samples CPU usage over a second, so an update is due one
	// second after the previous one started, not one second after it ended;
	// the wait only matters when a read returned early, such as a failure
	// before the sample.
	next := time.Now()
	for {
		select {
		case <-c.ctx.Request.Context().Done():
			return
		case <-time.After(time.Until(next)):
			// The timer is usually due already, so it can win the select
			// against a disconnect; check for one before sampling again.
			if c.ctx.Request.Context().Err() != nil {
				return
			}
			next = time.Now().Add(time.Second)
			func() {
				if flusher, ok := c.ctx.Writer.(http.Flusher); ok {
					defer flusher.Flush()
				}
				metrics, err := c.readMetrics()
				if err != nil {
					msg, _ := json.Marshal(map[string]string{ //nolint:errchkjson
						"error": err.Error(),
					})
					_, err = c.ctx.Writer.Write(append(msg, '\n'))
					if err != nil {
						log.Error("metrics: write %s: %v", string(msg), err)
					}
				} else {
					msg, _ := json.Marshal(metrics) //nolint:errchkjson
					_, err = c.ctx.Writer.Write(append(msg, '\n'))
					if err != nil {
						log.Error("metrics: write %s: %v", string(msg), err)
					}
				}
			}()
		}
	}
}

func (c *MetricController) readMetrics() (*model.Metrics, error) {
	metric := model.NewMetrics()

	metric.CpuCount = resources.CPUCount()
	cpuPercent, err := resources.CPUPercent(time.Second)
	if err != nil {
		return nil, fmt.Errorf("failed to get CPU percent: %w", err)
	}
	metric.CpuUsedPct = cpuPercent

	memStats, err := resources.MemoryStats()
	if err != nil {
		return nil, fmt.Errorf("failed to get memory info: %w", err)
	}
	metric.MemTotalMiB = float64(memStats.TotalBytes) / 1024 / 1024
	metric.MemUsedMiB = float64(memStats.UsedBytes) / 1024 / 1024

	return metric, nil
}
