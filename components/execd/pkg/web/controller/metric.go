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
	"runtime"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/shirou/gopsutil/cpu"
	"github.com/shirou/gopsutil/mem"

	"github.com/alibaba/opensandbox/execd/pkg/log"
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

	for {
		select {
		case <-c.ctx.Request.Context().Done():
			return
		case <-time.After(time.Second * 1):
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

	metric.CpuCount = float64(runtime.GOMAXPROCS(-1))
	cpuPercent, err := cpu.Percent(time.Second, false)
	if err != nil {
		return nil, fmt.Errorf("failed to get CPU percent: %w", err)
	}
	if len(cpuPercent) > 0 {
		metric.CpuUsedPct = cpuPercent[0]
	}

	vmStat, err := mem.VirtualMemory()
	if err != nil {
		return nil, fmt.Errorf("failed to get memory info: %w", err)
	}
	metric.MemTotalMiB = float64(vmStat.Total) / 1024 / 1024
	metric.MemUsedMiB = float64(vmStat.Used) / 1024 / 1024

	return metric, nil
}
