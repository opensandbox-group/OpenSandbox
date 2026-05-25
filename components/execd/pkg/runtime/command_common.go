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
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// tailStdPipe streams appended log data until the process finishes.
func (c *Controller) tailStdPipe(file string, onExecute func(text string), done <-chan struct{}) {
	lastPos := int64(0)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	mutex := &sync.Mutex{}
	var lastWasCR bool
	for {
		select {
		case <-done:
			c.readFromPos(mutex, file, lastPos, onExecute, true, &lastWasCR)
			return
		case <-ticker.C:
			newPos := c.readFromPos(mutex, file, lastPos, onExecute, false, &lastWasCR)
			lastPos = newPos
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
	logDir := os.TempDir()
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return nil, nil, fmt.Errorf("failed to create temp dir %s: %w", logDir, err)
	}

	stdout, err := os.OpenFile(c.stdoutFileName(session), os.O_RDWR|os.O_CREATE|os.O_TRUNC, os.ModePerm)
	if err != nil {
		return nil, nil, err
	}
	stderr, err := os.OpenFile(c.stderrFileName(session), os.O_RDWR|os.O_CREATE|os.O_TRUNC, os.ModePerm)
	if err != nil {
		stdout.Close()
		return nil, nil, err
	}

	return stdout, stderr, nil
}

func (c *Controller) combinedOutputDescriptor(session string) (io.WriteCloser, error) {
	logDir := os.TempDir()
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return nil, fmt.Errorf("failed to create temp dir %s: %w", logDir, err)
	}
	return os.OpenFile(c.combinedOutputFileName(session), os.O_RDWR|os.O_CREATE|os.O_TRUNC, os.ModePerm)
}

// stdoutFileName constructs the stdout log path.
func (c *Controller) stdoutFileName(session string) string {
	return filepath.Join(os.TempDir(), session+".stdout")
}

// stderrFileName constructs the stderr log path.
func (c *Controller) stderrFileName(session string) string {
	return filepath.Join(os.TempDir(), session+".stderr")
}

func (c *Controller) combinedOutputFileName(session string) string {
	return filepath.Join(os.TempDir(), session+".output")
}

// readFromPos streams new content from a file starting at startPos.
// lastWasCR persists CRLF detection across calls so a \r\n pair split between
// two polls does not surface a spurious blank line for the trailing \n.
func (c *Controller) readFromPos(mutex *sync.Mutex, filepath string, startPos int64, onExecute func(string), flushIncomplete bool, lastWasCR *bool) int64 {
	if !mutex.TryLock() {
		return -1
	}
	defer mutex.Unlock()

	file, err := os.Open(filepath)
	if err != nil {
		return startPos
	}
	defer file.Close()

	_, _ = file.Seek(startPos, 0) //nolint:errcheck

	reader := bufio.NewReader(file)
	var buffer bytes.Buffer
	var currentPos int64 = startPos
	cr := false
	if lastWasCR != nil {
		cr = *lastWasCR
	}
	defer func() {
		if lastWasCR != nil {
			*lastWasCR = cr
		}
	}()

	for {
		b, err := reader.ReadByte()
		if err != nil {
			if err == io.EOF {
				// If buffer has content but no newline, flush if needed, otherwise wait for next read
				if flushIncomplete && buffer.Len() > 0 {
					onExecute(buffer.String())
					buffer.Reset()
				}
			}
			break
		}
		currentPos++

		// Check if it's a line terminator (\n or \r)
		if b == '\n' || b == '\r' {
			switch {
			case buffer.Len() > 0:
				// Flush the line content without the terminator
				onExecute(buffer.String())
				buffer.Reset()
			case b == '\n' && cr:
				// Second half of a \r\n pair; already emitted on \r
			default:
				// Standalone blank line; surface it so callers see the gap
				onExecute("\n")
			}
			cr = (b == '\r')
			continue
		}

		cr = false
		buffer.WriteByte(b)
	}

	endPos, _ := file.Seek(0, 1)
	// If the last read position doesn't end with a newline, return buffer start position and wait for next flush
	if !flushIncomplete && buffer.Len() > 0 {
		return currentPos - int64(buffer.Len())
	}
	return endPos
}
