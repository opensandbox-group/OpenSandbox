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

package runtime

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/alibaba/opensandbox/internal/safego"

	"github.com/alibaba/opensandbox/execd/pkg/log"
)

const (
	commandOutputDirName       = "opensandbox-execd"
	commandOutputRetention     = 24 * time.Hour
	commandOutputSweepInterval = time.Hour
)

var legacyCommandOutputPattern = regexp.MustCompile(`^[0-9a-f]{32}\.(stdout|stderr|output)$`)

// tailStdPipe streams appended log data until the process finishes.
func (c *Controller) tailStdPipe(file string, onExecute func(text string), done <-chan struct{}) {
	var tail commandOutputTail
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-done:
			tail.read(file, onExecute, true)
			return
		case <-ticker.C:
			tail.read(file, onExecute, false)
		}
	}
}

// getCommandKernel retrieves a command execution context.
func (c *Controller) getCommandKernel(sessionID string) *commandKernel {
	if v, ok := c.commandClientMap.Load(sessionID); ok {
		if kernel, ok := v.(*commandKernel); ok {
			return kernel
		}
	}
	return nil
}

// storeCommandKernel registers a command execution context.
func (c *Controller) storeCommandKernel(sessionID string, kernel *commandKernel) {
	c.commandClientMap.Store(sessionID, kernel)
}

// stdLogDescriptor creates temporary files for capturing command output.
// It ensures the temp directory exists before opening files, so that commands
// continue to work even after the /tmp directory has been removed and recreated.
func (c *Controller) stdLogDescriptor(session string) (io.WriteCloser, io.WriteCloser, error) {
	logDir := c.commandOutputDir()
	if err := ensurePrivateCommandOutputDir(logDir); err != nil {
		return nil, nil, err
	}

	stdout, err := openNewCommandOutput(c.stdoutFileName(session))
	if err != nil {
		return nil, nil, err
	}
	stderr, err := openNewCommandOutput(c.stderrFileName(session))
	if err != nil {
		_ = stdout.Close()
		removeCommandOutputFiles(c.stdoutFileName(session))
		return nil, nil, err
	}

	return stdout, stderr, nil
}

func (c *Controller) combinedOutputDescriptor(session string) (io.WriteCloser, error) {
	logDir := c.commandOutputDir()
	if err := ensurePrivateCommandOutputDir(logDir); err != nil {
		return nil, err
	}
	return openNewCommandOutput(c.combinedOutputFileName(session))
}

func (c *Controller) commandOutputDir() string {
	return filepath.Join(os.TempDir(), commandOutputDirName)
}

// stdoutFileName constructs the stdout log path.
func (c *Controller) stdoutFileName(session string) string {
	return filepath.Join(c.commandOutputDir(), session+".stdout")
}

// stderrFileName constructs the stderr log path.
func (c *Controller) stderrFileName(session string) string {
	return filepath.Join(c.commandOutputDir(), session+".stderr")
}

func (c *Controller) combinedOutputFileName(session string) string {
	return filepath.Join(c.commandOutputDir(), session+".output")
}

func removeCommandOutputFiles(paths ...string) {
	seen := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		if path == "" {
			continue
		}
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			log.Warn("remove command output %s: %v", path, err)
		}
	}
}

func cleanupStaleCommandOutputFiles(dir string, cutoff time.Time, match func(string) bool, protected map[string]struct{}) {
	directory, err := os.Open(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Warn("read command output directory %s: %v", dir, err)
		}
		return
	}
	defer directory.Close()

	for {
		entries, readErr := directory.Readdir(256)
		for _, info := range entries {
			if !info.Mode().IsRegular() || !match(info.Name()) || !info.ModTime().Before(cutoff) {
				continue
			}
			path := filepath.Join(dir, info.Name())
			if _, ok := protected[path]; ok {
				continue
			}
			removeCommandOutputFiles(path)
		}
		if readErr != nil {
			if readErr != io.EOF {
				log.Warn("read command output directory %s: %v", dir, readErr)
			}
			return
		}
	}
}

func (c *Controller) protectedCommandOutputPaths() map[string]struct{} {
	protected := make(map[string]struct{})
	c.mu.RLock()
	c.commandClientMap.Range(func(_, value any) bool {
		kernel, ok := value.(*commandKernel)
		if !ok {
			return true
		}
		for _, path := range []string{kernel.stdoutPath, kernel.stderrPath} {
			if path != "" {
				protected[path] = struct{}{}
			}
		}
		return true
	})
	c.mu.RUnlock()
	return protected
}

func (c *Controller) cleanupFinishedCommands(cutoff time.Time) {
	var paths []string
	c.mu.Lock()
	c.commandClientMap.Range(func(key, value any) bool {
		kernel, ok := value.(*commandKernel)
		if !ok || kernel.running || kernel.finishedAt == nil || !kernel.finishedAt.Before(cutoff) {
			return true
		}

		paths = append(paths, kernel.stdoutPath, kernel.stderrPath)
		c.commandClientMap.Delete(key)
		return true
	})
	c.mu.Unlock()
	removeCommandOutputFiles(paths...)
}

func (c *Controller) cleanupOrphanedCommandOutputs(now time.Time) {
	cutoff := now.Add(-commandOutputRetention)
	protected := c.protectedCommandOutputPaths()
	if err := ensurePrivateCommandOutputDir(c.commandOutputDir()); err != nil {
		log.Warn("skip private command output cleanup: %v", err)
	} else {
		cleanupStaleCommandOutputFiles(c.commandOutputDir(), cutoff, legacyCommandOutputPattern.MatchString, protected)
	}
	cleanupStaleCommandOutputFiles(os.TempDir(), cutoff, legacyCommandOutputPattern.MatchString, nil)
}

// StartCommandOutputJanitor bounds command metadata and output retention and
// removes legacy files that older execd versions placed directly in /tmp.
func (c *Controller) StartCommandOutputJanitor(ctx context.Context) error {
	// Create and validate the private directory synchronously, before init mode
	// launches the sandbox workload. A workload may otherwise pre-create the
	// fixed temp path and make execd follow an unsafe directory or symlink.
	if err := ensurePrivateCommandOutputDir(c.commandOutputDir()); err != nil {
		return err
	}
	safego.Go(func() {
		c.cleanupOrphanedCommandOutputs(time.Now())
		c.cleanupFinishedCommands(time.Now().Add(-commandOutputRetention))
		ticker := time.NewTicker(commandOutputSweepInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				c.cleanupFinishedCommands(now.Add(-commandOutputRetention))
				c.cleanupOrphanedCommandOutputs(now)
			}
		}
	})
	return nil
}

// commandOutputTail retains an unfinished line while reading only appended bytes.
// Each stdout/stderr tail goroutine owns its own state.
type commandOutputTail struct {
	offset    int64
	pending   bytes.Buffer
	lastWasCR bool
}

func (t *commandOutputTail) read(path string, onExecute func(string), flushIncomplete bool) {
	file, err := os.Open(path)
	if err != nil {
		return
	}
	defer file.Close()

	if _, err := file.Seek(t.offset, io.SeekStart); err != nil {
		return
	}

	reader := bufio.NewReader(file)
	for {
		b, err := reader.ReadByte()
		if err != nil {
			if err == io.EOF && flushIncomplete && t.pending.Len() > 0 {
				onExecute(t.pending.String())
				t.pending.Reset()
			}
			break
		}
		t.offset++

		if b == '\n' || b == '\r' {
			switch {
			case t.pending.Len() > 0:
				onExecute(t.pending.String())
				t.pending.Reset()
			case b == '\n' && t.lastWasCR:
				// The preceding CR already emitted this line.
			default:
				onExecute("\n")
			}
			t.lastWasCR = b == '\r'
			continue
		}

		t.lastWasCR = false
		t.pending.WriteByte(b)
	}
	// Reuse storage within a poll, but release completed long lines between polls.
	if t.pending.Len() == 0 {
		t.pending = bytes.Buffer{}
	} else if t.pending.Cap() > 4096 && t.pending.Len() < t.pending.Cap()/2 {
		// Keep a short trailing fragment without retaining a completed long line's storage.
		t.pending = *bytes.NewBuffer(bytes.Clone(t.pending.Bytes()))
	}
}
