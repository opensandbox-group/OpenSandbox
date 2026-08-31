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

package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	mappedMemorySize    = 4096
	mappedValueCapacity = 512
	diskMarkerOffset    = 1 << 20
	diskMarkerSize      = 4096
	guestHTTPAddress    = ":8080"
	guestNetworkDevice  = "eth0"
	guestNetworkAddress = "10.0.2.15/24"
)

var (
	mappedMemoryMu sync.RWMutex
	mappedValue    []byte
)

type memoryStatus struct {
	PID     int    `json:"pid"`
	BootID  string `json:"boot_id"`
	Counter uint64 `json:"counter"`
	Value   string `json:"value"`
}

func mount(source, target, filesystem string) {
	if err := os.MkdirAll(target, 0o755); err != nil {
		return
	}
	_ = syscall.Mount(source, target, filesystem, 0, "")
}

func openConsole() *os.File {
	for i := 0; i < 100; i++ {
		console, err := os.OpenFile("/dev/ttyS0", os.O_RDWR, 0)
		if err == nil {
			return console
		}
		time.Sleep(10 * time.Millisecond)
	}
	return os.NewFile(uintptr(syscall.Stdout), "stdout")
}

func setMappedValue(value string) error {
	if value == "" || len(value) > mappedValueCapacity {
		return fmt.Errorf("value must contain 1-%d bytes", mappedValueCapacity)
	}
	mappedMemoryMu.Lock()
	defer mappedMemoryMu.Unlock()
	clear(mappedValue[:mappedValueCapacity])
	copy(mappedValue[:mappedValueCapacity], value)
	return nil
}

func getMappedValue() string {
	mappedMemoryMu.RLock()
	defer mappedMemoryMu.RUnlock()
	return mappedValueStringLocked()
}

func mappedValueStringLocked() string {
	value := mappedValue[:mappedValueCapacity]
	if index := strings.IndexByte(string(value), 0); index >= 0 {
		return string(value[:index])
	}
	return string(value)
}

func currentMemoryStatus() memoryStatus {
	mappedMemoryMu.RLock()
	defer mappedMemoryMu.RUnlock()
	return memoryStatus{
		PID:     os.Getpid(),
		BootID:  bootID(),
		Counter: binary.LittleEndian.Uint64(mappedValue[mappedMemorySize-8:]),
		Value:   mappedValueStringLocked(),
	}
}

func setDiskValue(value string) error {
	file, err := os.OpenFile("/dev/vda", os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	data := make([]byte, diskMarkerSize)
	copy(data, value)
	if _, err := file.WriteAt(data, diskMarkerOffset); err != nil {
		return err
	}
	return file.Sync()
}

func getDiskValue() (string, error) {
	file, err := os.Open("/dev/vda")
	if err != nil {
		return "", err
	}
	defer file.Close()
	data := make([]byte, diskMarkerSize)
	if _, err := file.ReadAt(data, diskMarkerOffset); err != nil {
		return "", err
	}
	if index := strings.IndexByte(string(data), 0); index >= 0 {
		data = data[:index]
	}
	return string(data), nil
}

func loadBlockDevice() error {
	output, err := exec.Command("/bin/busybox", "modprobe", "virtio_blk").CombinedOutput()
	if err != nil {
		return fmt.Errorf("modprobe virtio_blk: %w: %s", err, strings.TrimSpace(string(output)))
	}
	for i := 0; i < 300; i++ {
		if _, err := os.Stat("/dev/vda"); err == nil {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("/dev/vda was not created")
}

func configureNetwork() error {
	output, err := exec.Command("/bin/busybox", "modprobe", "virtio_net").CombinedOutput()
	if err != nil {
		return fmt.Errorf("modprobe virtio_net: %w: %s", err, strings.TrimSpace(string(output)))
	}
	for i := 0; i < 300; i++ {
		if _, err := os.Stat("/sys/class/net/" + guestNetworkDevice); err == nil {
			break
		}
		if i == 299 {
			return fmt.Errorf("network device %s was not created", guestNetworkDevice)
		}
		time.Sleep(10 * time.Millisecond)
	}
	commands := [][]string{
		{"ip", "link", "set", guestNetworkDevice, "up"},
		{"ip", "addr", "add", guestNetworkAddress, "dev", guestNetworkDevice},
	}
	for _, args := range commands {
		output, err := exec.Command("/bin/busybox", args...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("busybox %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
		}
	}
	return nil
}

func writeJSON(response http.ResponseWriter, status int, value any) {
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(value)
}

func newGuestHTTPHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			response.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		writeJSON(response, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("/status", func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			response.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		writeJSON(response, http.StatusOK, currentMemoryStatus())
	})
	mux.HandleFunc("/value", func(response http.ResponseWriter, request *http.Request) {
		switch request.Method {
		case http.MethodGet:
			writeJSON(response, http.StatusOK, map[string]string{"value": getMappedValue()})
		case http.MethodPut, http.MethodPost:
			data, err := io.ReadAll(io.LimitReader(request.Body, mappedValueCapacity+1))
			if err != nil {
				http.Error(response, err.Error(), http.StatusBadRequest)
				return
			}
			value := strings.TrimSpace(string(data))
			if err := setMappedValue(value); err != nil {
				http.Error(response, err.Error(), http.StatusBadRequest)
				return
			}
			writeJSON(response, http.StatusOK, map[string]string{"value": getMappedValue()})
		default:
			response.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/disk", func(response http.ResponseWriter, request *http.Request) {
		switch request.Method {
		case http.MethodGet:
			value, err := getDiskValue()
			if err != nil {
				http.Error(response, err.Error(), http.StatusInternalServerError)
				return
			}
			writeJSON(response, http.StatusOK, map[string]string{"value": value})
		case http.MethodPut, http.MethodPost:
			data, err := io.ReadAll(io.LimitReader(request.Body, mappedValueCapacity+1))
			if err != nil {
				http.Error(response, err.Error(), http.StatusBadRequest)
				return
			}
			value := strings.TrimSpace(string(data))
			if value == "" || len(value) > mappedValueCapacity {
				http.Error(response, fmt.Sprintf("value must contain 1-%d bytes", mappedValueCapacity), http.StatusBadRequest)
				return
			}
			if err := setDiskValue(value); err != nil {
				http.Error(response, err.Error(), http.StatusInternalServerError)
				return
			}
			writeJSON(response, http.StatusOK, map[string]string{"value": value})
		default:
			response.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	return mux
}

func startHTTPServer(console *os.File) error {
	listener, err := net.Listen("tcp", guestHTTPAddress)
	if err != nil {
		return err
	}
	go func() {
		if err := http.Serve(listener, newGuestHTTPHandler()); err != nil {
			fmt.Fprintf(console, "E2E_FATAL http-server=%q\n", err)
		}
	}()
	return nil
}

func bootID() string {
	value, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(value))
}

func main() {
	mount("devtmpfs", "/dev", "devtmpfs")
	mount("proc", "/proc", "proc")
	mount("sysfs", "/sys", "sysfs")

	console := openConsole()
	if console == nil {
		panic("failed to open guest serial console")
	}
	defer console.Close()
	if err := loadBlockDevice(); err != nil {
		fmt.Fprintf(console, "E2E_FATAL block-device=%q\n", err)
		for {
			time.Sleep(time.Hour)
		}
	}
	if err := configureNetwork(); err != nil {
		fmt.Fprintf(console, "E2E_FATAL network=%q\n", err)
		for {
			time.Sleep(time.Hour)
		}
	}

	var err error
	mappedValue, err = syscall.Mmap(-1, 0, mappedMemorySize,
		syscall.PROT_READ|syscall.PROT_WRITE,
		syscall.MAP_PRIVATE|syscall.MAP_ANON)
	if err != nil {
		fmt.Fprintf(console, "E2E_FATAL mmap=%q\n", err)
		for {
			time.Sleep(time.Hour)
		}
	}
	if err := setMappedValue("UNSET"); err != nil {
		panic(err)
	}

	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for range ticker.C {
			mappedMemoryMu.Lock()
			counterBytes := mappedValue[mappedMemorySize-8:]
			binary.LittleEndian.PutUint64(counterBytes, binary.LittleEndian.Uint64(counterBytes)+1)
			mappedMemoryMu.Unlock()
		}
	}()
	if err := startHTTPServer(console); err != nil {
		fmt.Fprintf(console, "E2E_FATAL http-listen=%q\n", err)
		for {
			time.Sleep(time.Hour)
		}
	}

	fmt.Fprintf(console, "E2E_READY pid=%d boot_id=%s http=%s\n", os.Getpid(), bootID(), guestHTTPAddress)
	for {
		time.Sleep(time.Hour)
	}
}
