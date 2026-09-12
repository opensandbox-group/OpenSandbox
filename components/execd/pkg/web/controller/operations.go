// Copyright 2025 Alibaba Group Holding Ltd.
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
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/alibaba/opensandbox/execd/pkg/runtime"
	"github.com/alibaba/opensandbox/execd/pkg/web/model"
)

type operationRunner interface {
	GetOperationInstance() runtime.OperationInstance
	GetOperation(string, string, string) (runtime.Operation, error)
	CreateCommandOperation(string, string, *runtime.ExecuteCodeRequest) (runtime.Operation, error)
	CreatePTYOperation(string, string, string, string) (runtime.Operation, error)
}

// decodeCreation preserves legacy decoding but rejects unrecognized semantics for
// keyed requests. Errors never echo a body, unknown property, or operation identity.
func (c *basicController) decodeCreation(target any, callerBound bool) error {
	if !callerBound {
		return c.bindJSON(target)
	}
	var raw json.RawMessage
	source := json.NewDecoder(http.MaxBytesReader(c.ctx.Writer, c.ctx.Request.Body, 1<<20))
	err := source.Decode(&raw)
	if err != nil {
		if errors.Is(err, io.EOF) {
			return io.EOF
		}
		return errors.New("invalid creation body")
	}
	if len(raw) == 0 {
		return io.EOF
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(raw, &fields); err != nil {
		return errors.New("invalid creation JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if callerBound {
		if len(raw) > 1<<20 {
			return errors.New("operation creation body exceeds 1 MiB")
		}
		var trailing any
		if source.Decode(&trailing) != io.EOF {
			return errors.New("unexpected trailing creation data")
		}
		var identity string
		if json.Unmarshal(fields["operation_id"], &identity) != nil || identity == "" {
			return errors.New("operation_id must be a nonempty string")
		}
		decoder.DisallowUnknownFields()
	}
	if request, ok := target.(*model.RunCommandRequest); ok {
		// UnmarshalJSON handles command/argv presence but bypasses the outer
		// decoder's unknown-field check. Check a method-free alias first,
		// keeping strict decoding confined to operation creation.
		type commandFields model.RunCommandRequest
		var checked commandFields
		if err = decoder.Decode(&checked); err != nil {
			return errors.New("invalid creation fields")
		}
		if err = json.Unmarshal(raw, request); err != nil {
			return errors.New("invalid creation fields")
		}
		return nil
	}
	if err = decoder.Decode(target); err != nil {
		return errors.New("invalid creation fields")
	}
	return nil
}

func (c *basicController) operationResult(op runtime.Operation, err error) {
	c.ctx.Header("Cache-Control", "no-store")
	if err != nil {
		var e *runtime.OperationError
		if !errors.As(err, &e) {
			c.RespondError(500, model.ErrorCodeRuntimeError, "operation creation failed")
			return
		}
		status := http.StatusBadRequest
		switch e.Code {
		case "operation_conflict", "operation_instance_mismatch":
			status = http.StatusConflict
		case "operation_expired":
			status = http.StatusGone
		case "operation_not_found":
			status = http.StatusNotFound
		case "operation_capacity_exceeded":
			status = http.StatusServiceUnavailable
		}
		c.RespondError(status, model.ErrorCode(e.Code), e.Message)
		return
	}
	status := http.StatusOK
	if op.State == "creating" {
		status = http.StatusAccepted
	}
	c.ctx.JSON(status, op)
}

func (c *basicController) operations() operationRunner {
	runner, ok := codeRunner.(operationRunner)
	if !ok {
		c.RespondError(http.StatusNotImplemented, model.ErrorCodeNotSupported, "operation creation unavailable")
	}
	return runner
}

func (c *CodeInterpretingController) GetOperationInstance() {
	c.ctx.Header("Cache-Control", "no-store")
	if runner := c.operations(); runner != nil {
		c.RespondSuccess(runner.GetOperationInstance())
	}
}

func (c *CodeInterpretingController) GetOperation() {
	if runner := c.operations(); runner != nil {
		op, err := runner.GetOperation(c.ctx.GetString("operationPrincipal"), c.ctx.Query("kind"), c.ctx.GetHeader("X-EXECD-OPERATION-ID"))
		c.operationResult(op, err)
	}
}
