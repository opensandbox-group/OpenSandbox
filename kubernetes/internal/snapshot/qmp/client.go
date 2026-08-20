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

package qmp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"syscall"
	"time"
)

const migrationFDName = "opensandbox-vmstate"

// Client is a small synchronous QMP client. Snapshot commands run serially,
// while asynchronous QMP events are ignored until the matching command reply
// arrives.
type Client struct {
	conn    *net.UnixConn
	decoder *json.Decoder
	nextID  uint64
}

type message struct {
	Greeting json.RawMessage `json:"QMP,omitempty"`
	Return   json.RawMessage `json:"return,omitempty"`
	Error    *commandError   `json:"error,omitempty"`
	Event    string          `json:"event,omitempty"`
	ID       string          `json:"id,omitempty"`
}

type commandError struct {
	Class string `json:"class"`
	Desc  string `json:"desc"`
}

func (e *commandError) Error() string {
	if e.Class == "" {
		return e.Desc
	}
	return fmt.Sprintf("%s: %s", e.Class, e.Desc)
}

// MigrationStatus is the subset of query-migrate used by checkpoint export.
type MigrationStatus struct {
	Status    string `json:"status"`
	ErrorDesc string `json:"error-desc,omitempty"`
}

type RunState struct {
	Status  string `json:"status"`
	Running bool   `json:"running"`
}

// VersionInfo is the stable subset of query-version exposed to callers.
type VersionInfo struct {
	QEMU struct {
		Major int `json:"major"`
		Minor int `json:"minor"`
		Micro int `json:"micro"`
	} `json:"qemu"`
	Package string `json:"package"`
}

func (v VersionInfo) String() string {
	return fmt.Sprintf("%d.%d.%d", v.QEMU.Major, v.QEMU.Minor, v.QEMU.Micro)
}

// Dial connects to a QMP Unix socket and enables QMP capabilities.
func Dial(ctx context.Context, socketPath string) (*Client, error) {
	dialer := net.Dialer{}
	connection, err := dialer.DialContext(ctx, "unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("dial QMP socket %q: %w", socketPath, err)
	}

	unixConn, ok := connection.(*net.UnixConn)
	if !ok {
		connection.Close()
		return nil, fmt.Errorf("QMP connection %q is not a Unix connection", socketPath)
	}

	client := &Client{conn: unixConn, decoder: json.NewDecoder(unixConn)}
	if err := client.setDeadline(ctx); err != nil {
		client.Close()
		return nil, err
	}

	var greeting message
	if err := client.decoder.Decode(&greeting); err != nil {
		client.Close()
		return nil, fmt.Errorf("read QMP greeting: %w", err)
	}
	if len(greeting.Greeting) == 0 {
		client.Close()
		return nil, errors.New("QMP peer did not send a greeting")
	}
	if err := client.Execute(ctx, "qmp_capabilities", nil, nil, nil); err != nil {
		client.Close()
		return nil, fmt.Errorf("enable QMP capabilities: %w", err)
	}
	return client, nil
}

func (c *Client) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

// Execute runs one QMP command. When fd is non-nil it is attached to the JSON
// command using SCM_RIGHTS, as required by QMP getfd.
func (c *Client) Execute(ctx context.Context, command string, arguments any, fd *os.File, result any) error {
	if err := c.setDeadline(ctx); err != nil {
		return err
	}

	c.nextID++
	id := "opensandbox-" + strconv.FormatUint(c.nextID, 10)
	request := map[string]any{"execute": command, "id": id}
	if arguments != nil {
		request["arguments"] = arguments
	}
	payload, err := json.Marshal(request)
	if err != nil {
		return fmt.Errorf("marshal QMP command %q: %w", command, err)
	}
	payload = append(payload, '\r', '\n')

	if fd == nil {
		if _, err := c.conn.Write(payload); err != nil {
			return fmt.Errorf("write QMP command %q: %w", command, err)
		}
	} else {
		if _, _, err := c.conn.WriteMsgUnix(payload, syscall.UnixRights(int(fd.Fd())), nil); err != nil {
			return fmt.Errorf("write QMP command %q with file descriptor: %w", command, err)
		}
	}

	for {
		var response message
		if err := c.decoder.Decode(&response); err != nil {
			return fmt.Errorf("read QMP command %q response: %w", command, err)
		}
		if response.ID != id {
			continue
		}
		if response.Error != nil {
			return fmt.Errorf("QMP command %q failed: %w", command, response.Error)
		}
		if result != nil && len(response.Return) > 0 {
			if err := json.Unmarshal(response.Return, result); err != nil {
				return fmt.Errorf("decode QMP command %q response: %w", command, err)
			}
		}
		return nil
	}
}

// Version returns the QEMU version reported by the connected process.
func (c *Client) Version(ctx context.Context) (VersionInfo, error) {
	var version VersionInfo
	err := c.Execute(ctx, "query-version", nil, nil, &version)
	return version, err
}

// Continue resumes a source VM that was left in postmigrate after a later
// snapshot publication step failed.
func (c *Client) Continue(ctx context.Context) error {
	var state RunState
	if err := c.Execute(ctx, "query-status", nil, nil, &state); err != nil {
		return err
	}
	if state.Running {
		return nil
	}
	if state.Status != "postmigrate" && state.Status != "paused" && state.Status != "prelaunch" {
		return fmt.Errorf("source QEMU cannot resume from state %q", state.Status)
	}
	return c.Execute(ctx, "cont", nil, nil, nil)
}

// ExportMigration sends outputFD to QEMU and waits until the migration stream
// has been completely written. Successful migration leaves QEMU in postmigrate.
func (c *Client) ExportMigration(ctx context.Context, outputFD *os.File, pollInterval time.Duration) error {
	if outputFD == nil {
		return errors.New("migration output file descriptor is required")
	}
	if pollInterval <= 0 {
		pollInterval = 100 * time.Millisecond
	}

	if err := c.Execute(ctx, "getfd", map[string]string{"fdname": migrationFDName}, outputFD, nil); err != nil {
		return err
	}
	defer func() {
		cleanupContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = c.Execute(cleanupContext, "closefd", map[string]string{"fdname": migrationFDName}, nil, nil)
	}()

	if err := c.Execute(ctx, "migrate", map[string]string{"uri": "fd:" + migrationFDName}, nil, nil); err != nil {
		return err
	}

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		var status MigrationStatus
		if err := c.Execute(ctx, "query-migrate", nil, nil, &status); err != nil {
			c.cancelMigration()
			return err
		}
		switch status.Status {
		case "completed":
			return nil
		case "failed":
			if status.ErrorDesc == "" {
				status.ErrorDesc = "QEMU migration failed without an error description"
			}
			return errors.New(status.ErrorDesc)
		case "cancelled": //nolint:misspell // QEMU QMP reports the British spelling
			return errors.New("QEMU migration was canceled")
		case "setup", "active", "postcopy-active", "pre-switchover", "device", "wait-unplug", "colo":
			// Still progressing.
		case "none":
			return errors.New("QEMU migration stopped before producing a checkpoint")
		default:
			return fmt.Errorf("unsupported QEMU migration status %q", status.Status)
		}

		select {
		case <-ctx.Done():
			c.cancelMigration()
			return fmt.Errorf("wait for QEMU migration: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func (c *Client) cancelMigration() {
	cleanupContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = c.Execute(cleanupContext, "migrate_cancel", nil, nil, nil)
}

func (c *Client) setDeadline(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		return c.conn.SetDeadline(time.Time{})
	}
	return c.conn.SetDeadline(deadline)
}
