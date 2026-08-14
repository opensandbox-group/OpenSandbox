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
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestExportMigrationPassesFDAndWaitsForCompletion(t *testing.T) {
	socketDir, err := os.MkdirTemp("/tmp", "osb-qmp-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socketPath := filepath.Join(socketDir, "qmp.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	serverErr := make(chan error, 1)
	go func() { serverErr <- serveFakeQMP(listener) }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := Dial(ctx, socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()

	if err := client.ExportMigration(ctx, writer, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, len("stream"))
	if _, err := io.ReadFull(reader, buffer); err != nil {
		t.Fatal(err)
	}
	if string(buffer) != "stream" {
		t.Fatalf("unexpected migration bytes %q", buffer)
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
}

func serveFakeQMP(listener *net.UnixListener) error {
	connection, err := listener.AcceptUnix()
	if err != nil {
		return err
	}
	defer connection.Close()
	if _, err := connection.Write([]byte(`{"QMP":{"version":{"qemu":{"major":9,"minor":1,"micro":0}}}}` + "\r\n")); err != nil {
		return err
	}

	queryCount := 0
	receivedFD := -1
	for {
		payload := make([]byte, 4096)
		oob := make([]byte, 4096)
		n, oobn, _, _, err := connection.ReadMsgUnix(payload, oob)
		if err != nil {
			return err
		}
		var request struct {
			Execute string `json:"execute"`
			ID      string `json:"id"`
		}
		if err := json.Unmarshal(payload[:n], &request); err != nil {
			return fmt.Errorf("decode request: %w", err)
		}

		if request.Execute == "getfd" {
			messages, err := syscall.ParseSocketControlMessage(oob[:oobn])
			if err != nil {
				return err
			}
			for _, message := range messages {
				fds, err := syscall.ParseUnixRights(&message)
				if err != nil {
					return err
				}
				if len(fds) > 0 {
					receivedFD = fds[0]
				}
			}
			if receivedFD < 0 {
				return fmt.Errorf("getfd request did not include SCM_RIGHTS")
			}
			if _, err := os.NewFile(uintptr(receivedFD), "migration").Write([]byte("stream")); err != nil {
				return err
			}
		}

		response := any(map[string]any{})
		if request.Execute == "query-migrate" {
			queryCount++
			status := "active"
			if queryCount > 1 {
				status = "completed"
			}
			response = map[string]string{"status": status}
		}
		data, err := json.Marshal(map[string]any{"return": response, "id": request.ID})
		if err != nil {
			return err
		}
		if _, err := connection.Write(append(data, '\r', '\n')); err != nil {
			return err
		}
		if request.Execute == "closefd" {
			if receivedFD >= 0 {
				_ = syscall.Close(receivedFD)
			}
			return nil
		}
	}
}
