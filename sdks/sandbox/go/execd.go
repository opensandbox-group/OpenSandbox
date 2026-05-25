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

package opensandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"strconv"
)

// ExecdClient provides access to the OpenSandbox Execd API for code execution,
// command execution, file operations, and system metrics.
type ExecdClient struct {
	client *Client
}

// execdAuthHeader is the authentication header used by the Execd API.
const execdAuthHeader = "X-EXECD-ACCESS-TOKEN"

// NewExecdClient creates a new ExecdClient for the given base URL and access token.
func NewExecdClient(baseURL, accessToken string, opts ...Option) *ExecdClient {
	return &ExecdClient{
		client: NewClient(baseURL, accessToken, execdAuthHeader, opts...),
	}
}

// Ping verifies that the Execd server is running and responsive.
func (e *ExecdClient) Ping(ctx context.Context) error {
	return e.client.doRequest(ctx, http.MethodGet, "/ping", nil, nil)
}

// ListContexts returns all active code execution contexts for the given language.
func (e *ExecdClient) ListContexts(ctx context.Context, language string) ([]CodeContext, error) {
	var result []CodeContext
	params := url.Values{}
	params.Set("language", language)
	path := "/code/contexts?" + params.Encode()
	err := e.client.doRequest(ctx, http.MethodGet, path, nil, &result)
	return result, err
}

// CreateContext creates a new code execution context and returns its context ID.
func (e *ExecdClient) CreateContext(ctx context.Context, req CreateContextRequest) (*CodeContext, error) {
	var result CodeContext
	err := e.client.doRequest(ctx, http.MethodPost, "/code/context", req, &result)
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// GetContext retrieves the details of an existing code execution context by ID.
func (e *ExecdClient) GetContext(ctx context.Context, contextID string) (*CodeContext, error) {
	var result CodeContext
	path := "/code/contexts/" + url.PathEscape(contextID)
	err := e.client.doRequest(ctx, http.MethodGet, path, nil, &result)
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// DeleteContext deletes a code execution context by ID.
func (e *ExecdClient) DeleteContext(ctx context.Context, contextID string) error {
	path := "/code/contexts/" + url.PathEscape(contextID)
	return e.client.doRequest(ctx, http.MethodDelete, path, nil, nil)
}

// DeleteContextsByLanguage deletes all code execution contexts for the given language.
func (e *ExecdClient) DeleteContextsByLanguage(ctx context.Context, language string) error {
	params := url.Values{}
	params.Set("language", language)
	path := "/code/contexts?" + params.Encode()
	return e.client.doRequest(ctx, http.MethodDelete, path, nil, nil)
}

// ExecuteCode executes code in the specified context and streams output events
// via SSE. The handler is called for each event received from the server.
func (e *ExecdClient) ExecuteCode(ctx context.Context, req RunCodeRequest, handler EventHandler) error {
	return e.client.doStreamRequest(ctx, http.MethodPost, "/code", req, handler)
}

// InterruptCode interrupts the currently running code execution.
func (e *ExecdClient) InterruptCode(ctx context.Context, sessionID string) error {
	params := url.Values{}
	params.Set("id", sessionID)
	path := "/code?" + params.Encode()
	return e.client.doRequest(ctx, http.MethodDelete, path, nil, nil)
}

// CreateSession creates a new bash session and returns it with a session ID.
func (e *ExecdClient) CreateSession(ctx context.Context) (*Session, error) {
	var result Session
	err := e.client.doRequest(ctx, http.MethodPost, "/session", struct{}{}, &result)
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// RunInSession executes a command in an existing bash session and streams
// output events via SSE.
func (e *ExecdClient) RunInSession(ctx context.Context, sessionID string, req RunInSessionRequest, handler EventHandler) error {
	path := "/session/" + url.PathEscape(sessionID) + "/run"
	return e.client.doStreamRequest(ctx, http.MethodPost, path, req, handler)
}

// DeleteSession deletes a bash session by ID.
func (e *ExecdClient) DeleteSession(ctx context.Context, sessionID string) error {
	path := "/session/" + url.PathEscape(sessionID)
	return e.client.doRequest(ctx, http.MethodDelete, path, nil, nil)
}

// RunCommand executes a shell command and streams output events via SSE.
func (e *ExecdClient) RunCommand(ctx context.Context, req RunCommandRequest, handler EventHandler) error {
	return e.client.doStreamRequest(ctx, http.MethodPost, "/command", req, handler)
}

// InterruptCommand interrupts the currently running command execution.
func (e *ExecdClient) InterruptCommand(ctx context.Context, sessionID string) error {
	params := url.Values{}
	params.Set("id", sessionID)
	path := "/command?" + params.Encode()
	return e.client.doRequest(ctx, http.MethodDelete, path, nil, nil)
}

// GetCommandStatus returns the current status of a command by ID.
func (e *ExecdClient) GetCommandStatus(ctx context.Context, commandID string) (*CommandStatusResponse, error) {
	var result CommandStatusResponse
	path := "/command/status/" + url.PathEscape(commandID)
	err := e.client.doRequest(ctx, http.MethodGet, path, nil, &result)
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// GetCommandLogs returns stdout/stderr for a background command. Pass cursor=-1
// or cursor=0 for the full log. The returned CommandLogsResponse includes the
// tail cursor for incremental polling.
func (e *ExecdClient) GetCommandLogs(ctx context.Context, commandID string, cursor *int64) (*CommandLogsResponse, error) {
	path := "/command/" + url.PathEscape(commandID) + "/logs"
	if cursor != nil {
		path += "?cursor=" + strconv.FormatInt(*cursor, 10)
	}

	var result *CommandLogsResponse
	err := e.client.withRetry(ctx, func() error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.client.baseURL+path, nil)
		if err != nil {
			return fmt.Errorf("opensandbox: create request: %w", err)
		}
		req.Header.Set("User-Agent", "OpenSandbox-Go-SDK/"+Version)
		for k, v := range e.client.headers {
			req.Header.Set(k, v)
		}
		if e.client.apiKey != "" {
			req.Header.Set(e.client.authHeader, e.client.apiKey)
		}
		req.Header.Set("Accept", "text/plain")

		resp, err := e.client.httpClient.Do(req)
		if err != nil {
			return fmt.Errorf("opensandbox: do request: %w", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode >= 400 {
			return handleError(resp)
		}

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return fmt.Errorf("opensandbox: read response: %w", err)
		}

		logResp := &CommandLogsResponse{
			Output: string(body),
		}
		if cursorStr := resp.Header.Get("EXECD-COMMANDS-TAIL-CURSOR"); cursorStr != "" {
			parsed, parseErr := strconv.ParseInt(cursorStr, 10, 64)
			if parseErr != nil {
				return fmt.Errorf("opensandbox: invalid EXECD-COMMANDS-TAIL-CURSOR header %q: %w", cursorStr, parseErr)
			}
			logResp.Cursor = parsed
		}
		result = logResp
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// GetFileInfo retrieves metadata for the file at the given path.
func (e *ExecdClient) GetFileInfo(ctx context.Context, path string) (map[string]FileInfo, error) {
	var result map[string]FileInfo
	params := url.Values{}
	params.Set("path", path)
	reqPath := "/files/info?" + params.Encode()
	err := e.client.doRequest(ctx, http.MethodGet, reqPath, nil, &result)
	return result, err
}

// DeleteFiles deletes one or more files from the sandbox.
func (e *ExecdClient) DeleteFiles(ctx context.Context, paths []string) error {
	params := url.Values{}
	for _, p := range paths {
		params.Add("path", p)
	}
	reqPath := "/files?" + params.Encode()
	return e.client.doRequest(ctx, http.MethodDelete, reqPath, nil, nil)
}

// SetPermissions changes permissions, owner, and group for the specified files.
func (e *ExecdClient) SetPermissions(ctx context.Context, req PermissionsRequest) error {
	return e.client.doRequest(ctx, http.MethodPost, "/files/permissions", req, nil)
}

// MoveFiles renames or moves files to new paths.
func (e *ExecdClient) MoveFiles(ctx context.Context, req MoveRequest) error {
	return e.client.doRequest(ctx, http.MethodPost, "/files/mv", req, nil)
}

// SearchFiles searches for files matching a glob pattern within a directory.
func (e *ExecdClient) SearchFiles(ctx context.Context, dir string, pattern string) ([]FileInfo, error) {
	var result []FileInfo
	params := url.Values{}
	params.Set("path", dir)
	if pattern != "" {
		params.Set("pattern", pattern)
	}
	reqPath := "/files/search?" + params.Encode()
	err := e.client.doRequest(ctx, http.MethodGet, reqPath, nil, &result)
	return result, err
}

// ReplaceInFiles performs text replacement in the specified files.
func (e *ExecdClient) ReplaceInFiles(ctx context.Context, req ReplaceRequest) error {
	return e.client.doRequest(ctx, http.MethodPost, "/files/replace", req, nil)
}

// UploadFileOptions configures the destination path and multipart filename for an upload.
type UploadFileOptions struct {
	FileName string
	Metadata FileMetadata
}

// UploadFileEntry describes one file part in a multi-file upload request.
type UploadFileEntry struct {
	File    io.Reader
	Options UploadFileOptions
}

// UploadFile uploads a single file to the sandbox.
func (e *ExecdClient) UploadFile(ctx context.Context, file io.Reader, opts UploadFileOptions) error {
	return e.UploadFiles(ctx, []UploadFileEntry{{File: file, Options: opts}})
}

// UploadFiles uploads one or more files to the sandbox in a single multipart request.
func (e *ExecdClient) UploadFiles(ctx context.Context, entries []UploadFileEntry) error {
	req, bodyCloser, err := e.newUploadFilesRequest(ctx, entries)
	if err != nil {
		return err
	}
	defer bodyCloser.Close()

	req.Header.Set("User-Agent", "OpenSandbox-Go-SDK/"+Version)
	for k, v := range e.client.headers {
		req.Header.Set(k, v)
	}
	if e.client.apiKey != "" {
		req.Header.Set(e.client.authHeader, e.client.apiKey)
	}

	resp, err := e.client.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("opensandbox: do request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return handleError(resp)
	}
	return nil
}

func (e *ExecdClient) newUploadFilesRequest(ctx context.Context, entries []UploadFileEntry) (*http.Request, io.Closer, error) {
	if len(entries) == 0 {
		return nil, nil, &InvalidArgumentError{Field: "entries", Message: "at least one file entry is required"}
	}
	for i, entry := range entries {
		if entry.File == nil {
			return nil, nil, &InvalidArgumentError{Field: fmt.Sprintf("entries[%d].file", i), Message: "file reader is required"}
		}
		if entry.Options.Metadata.Path == "" {
			return nil, nil, &InvalidArgumentError{Field: fmt.Sprintf("entries[%d].metadata.path", i), Message: "path is required"}
		}
	}

	pr, pw := io.Pipe()
	writer := multipart.NewWriter(pw)
	contentType := writer.FormDataContentType()

	go func() {
		for i, entry := range entries {
			metaJSON, err := json.Marshal(entry.Options.Metadata)
			if err != nil {
				_ = pw.CloseWithError(fmt.Errorf("opensandbox: marshal metadata for entry %d: %w", i, err))
				return
			}
			metaHeader := make(textproto.MIMEHeader)
			metaHeader.Set("Content-Disposition", `form-data; name="metadata"; filename="metadata"`)
			metaHeader.Set("Content-Type", "application/json")
			metaPart, err := writer.CreatePart(metaHeader)
			if err != nil {
				_ = pw.CloseWithError(fmt.Errorf("opensandbox: create metadata part for entry %d: %w", i, err))
				return
			}
			if _, err := metaPart.Write(metaJSON); err != nil {
				_ = pw.CloseWithError(fmt.Errorf("opensandbox: write metadata for entry %d: %w", i, err))
				return
			}

			fileName := entry.Options.FileName
			if fileName == "" {
				fileName = "file"
			}
			filePart, err := writer.CreateFormFile("file", fileName)
			if err != nil {
				_ = pw.CloseWithError(fmt.Errorf("opensandbox: create file part for entry %d: %w", i, err))
				return
			}
			if _, err := io.Copy(filePart, entry.File); err != nil {
				_ = pw.CloseWithError(fmt.Errorf("opensandbox: write file for entry %d: %w", i, err))
				return
			}
		}
		if err := writer.Close(); err != nil {
			_ = pw.CloseWithError(fmt.Errorf("opensandbox: close multipart: %w", err))
			return
		}
		_ = pw.Close()
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.client.baseURL+"/files/upload", pr)
	if err != nil {
		_ = pr.Close()
		return nil, nil, fmt.Errorf("opensandbox: create request: %w", err)
	}
	req.Header.Set("Content-Type", contentType)
	return req, pr, nil
}

// DownloadFile downloads a file from the sandbox. The caller must close the
// returned io.ReadCloser. Pass rangeHeader (e.g. "bytes=0-1023") for partial
// content, or empty string for the full file.
func (e *ExecdClient) DownloadFile(ctx context.Context, remotePath string, rangeHeader string) (io.ReadCloser, error) {
	params := url.Values{}
	params.Set("path", remotePath)
	reqPath := "/files/download?" + params.Encode()

	var resp *http.Response
	err := e.client.withRetry(ctx, func() error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.client.baseURL+reqPath, nil)
		if err != nil {
			return fmt.Errorf("opensandbox: create request: %w", err)
		}
		req.Header.Set("User-Agent", "OpenSandbox-Go-SDK/"+Version)
		for k, v := range e.client.headers {
			req.Header.Set(k, v)
		}
		if e.client.apiKey != "" {
			req.Header.Set(e.client.authHeader, e.client.apiKey)
		}
		if rangeHeader != "" {
			req.Header.Set("Range", rangeHeader)
		}

		r, err := e.client.httpClient.Do(req)
		if err != nil {
			return fmt.Errorf("opensandbox: do request: %w", err)
		}
		if r.StatusCode >= 400 {
			defer r.Body.Close()
			return handleError(r)
		}
		resp = r
		return nil
	})
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// CreateDirectory creates a directory at the given path with the specified mode.
// Parent directories are created as needed (like mkdir -p).
// Mode is specified as octal digits in decimal form (e.g. 755 for rwxr-xr-x).
// Use OctalMode() to convert Go os.FileMode to the expected format.
func (e *ExecdClient) CreateDirectory(ctx context.Context, path string, mode int) error {
	body := map[string]map[string]int{
		path: {"mode": mode},
	}
	return e.client.doRequest(ctx, http.MethodPost, "/directories", body, nil)
}

// OctalMode converts a Go os.FileMode to the octal-digits-as-int format
// expected by the OpenSandbox server (e.g. os.FileMode(0755) -> 755).
func OctalMode(m os.FileMode) int {
	v, _ := strconv.Atoi(fmt.Sprintf("%o", m))
	return v
}

// DeleteDirectory deletes a directory and all its contents recursively.
func (e *ExecdClient) DeleteDirectory(ctx context.Context, path string) error {
	params := url.Values{}
	params.Set("path", path)
	reqPath := "/directories?" + params.Encode()
	return e.client.doRequest(ctx, http.MethodDelete, reqPath, nil, nil)
}

// GetMetrics retrieves current system resource metrics.
func (e *ExecdClient) GetMetrics(ctx context.Context) (*Metrics, error) {
	var result Metrics
	err := e.client.doRequest(ctx, http.MethodGet, "/metrics", nil, &result)
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// WatchMetrics streams system metrics in real-time via SSE. The handler
// receives a new event approximately every second until the context is
// cancelled or an error occurs.
func (e *ExecdClient) WatchMetrics(ctx context.Context, handler EventHandler) error {
	return e.client.doStreamRequest(ctx, http.MethodGet, "/metrics/watch", nil, handler)
}
