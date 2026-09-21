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

package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

func main() {
	ctx := context.Background()
	if err := run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	vnc := &VNCServer{}

	errs := make(chan error, 10)

	go func() {
		if err := vnc.Run(ctx); err != nil {
			log.Println(err, "VNC server exited with error")
			errs <- fmt.Errorf("VNC server exited with error: %w", err)
			cancel()
		}
	}()

	if err := vnc.WaitForReady(ctx); err != nil {
		return fmt.Errorf("failed to wait for VNC server: %w", err)
	}

	chrome := &Chrome{}
	go func() {
		if err := chrome.Run(ctx); err != nil {
			log.Println(err, "Chrome exited with error")
			errs <- fmt.Errorf("Chrome exited with error: %w", err)
			cancel()
		}
	}()

	if err := chrome.WaitForReady(ctx); err != nil {
		return fmt.Errorf("failed to wait for Chrome: %w", err)
	}
	log.Println("Chrome and VNC server are running")

	<-ctx.Done()
	errs <- ctx.Err()

	// Return the first error (or nil))
	return <-errs
}

type Chrome struct {
}

func (c *Chrome) Run(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "/chrome.sh")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	var env []string
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "DISPLAY=") {
			continue
		}
		env = append(env, e)
	}
	env = append(env, "DISPLAY=:1")
	cmd.Env = env

	return cmd.Run()
}

func (c *Chrome) WaitForReady(ctx context.Context) error {
	u := "http://localhost:9222/json/version"

	httpClient := &http.Client{}
	httpClient.Timeout = 200 * time.Millisecond

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
		if err != nil {
			return fmt.Errorf("failed to create HTTP request: %w", err)
		}

		// Send the HTTP request
		response, err := httpClient.Do(req)
		if err != nil {
			log.Println("Waiting for Chrome to be ready", "url", u, "info", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		defer response.Body.Close()

		// Check for HTTP 200 OK
		if response.StatusCode != http.StatusOK {
			log.Println("Waiting for Chrome to be ready", "url", u, "status", response.Status)
			time.Sleep(100 * time.Millisecond)
			continue
		}

		b, err := io.ReadAll(response.Body)
		if err != nil {
			log.Println("Waiting for Chrome to be ready", "url", u, "info", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}

		log.Println("Chrome is ready", "url", u, "response", string(b))
		break
	}
	return nil
}

type VNCServer struct {
}

func (v *VNCServer) Run(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "Xtigervnc", ":1", "-geometry", "1280x1024")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	log.Println("Starting VNC server", "command", cmd.String())
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start VNC server: %w", err)
	}

	go func() {
		<-ctx.Done()
		if err := cmd.Process.Kill(); err != nil {
			log.Println(err, "failed to kill VNC server")
		}
	}()

	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("VNC server exited with error: %w", err)
	}

	return nil
}

func (v *VNCServer) WaitForReady(ctx context.Context) error {
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		cmd := exec.CommandContext(ctx, "xdpyinfo", "-display", ":1")
		var stdout bytes.Buffer
		cmd.Stdout = &stdout
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			log.Println("Waiting for VNC server to be ready", "info", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}

		log.Println("VNC is ready")
		break
	}
	return nil
}
