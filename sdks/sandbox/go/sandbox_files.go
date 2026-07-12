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
	"fmt"
	"io"
)

// GetFileInfo retrieves file metadata.
func (s *Sandbox) GetFileInfo(ctx context.Context, path string) (map[string]FileInfo, error) {
	if s.execd == nil {
		return nil, fmt.Errorf("opensandbox: execd client not initialized")
	}
	return s.execd.GetFileInfo(ctx, path)
}

// DeleteFiles deletes one or more files from the sandbox.
func (s *Sandbox) DeleteFiles(ctx context.Context, paths []string) error {
	if s.execd == nil {
		return fmt.Errorf("opensandbox: execd client not initialized")
	}
	return s.execd.DeleteFiles(ctx, paths)
}

// MoveFiles renames or moves files.
func (s *Sandbox) MoveFiles(ctx context.Context, req MoveRequest) error {
	if s.execd == nil {
		return fmt.Errorf("opensandbox: execd client not initialized")
	}
	return s.execd.MoveFiles(ctx, req)
}

// SearchFiles searches for files matching a pattern.
func (s *Sandbox) SearchFiles(ctx context.Context, dir, pattern string) ([]FileInfo, error) {
	if s.execd == nil {
		return nil, fmt.Errorf("opensandbox: execd client not initialized")
	}
	return s.execd.SearchFiles(ctx, dir, pattern)
}

// ListDirectory lists the immediate children of the given directory using
// the server-side default depth (1). Use ListDirectoryWithDepth to override.
func (s *Sandbox) ListDirectory(ctx context.Context, path string) ([]FileInfo, error) {
	if s.execd == nil {
		return nil, fmt.Errorf("opensandbox: execd client not initialized")
	}
	return s.execd.ListDirectory(ctx, path)
}

// ListDirectoryWithDepth lists directory contents up to the given depth.
// depth=0 returns an empty slice; depth=1 lists immediate children; larger
// values include descendants up to that many levels below path.
func (s *Sandbox) ListDirectoryWithDepth(ctx context.Context, path string, depth int) ([]FileInfo, error) {
	if s.execd == nil {
		return nil, fmt.Errorf("opensandbox: execd client not initialized")
	}
	return s.execd.ListDirectoryWithDepth(ctx, path, depth)
}

// SetPermissions changes file permissions.
func (s *Sandbox) SetPermissions(ctx context.Context, req PermissionsRequest) error {
	if s.execd == nil {
		return fmt.Errorf("opensandbox: execd client not initialized")
	}
	return s.execd.SetPermissions(ctx, req)
}

// UploadFile uploads a single file to the sandbox.
func (s *Sandbox) UploadFile(ctx context.Context, file io.Reader, opts UploadFileOptions) error {
	if s.execd == nil {
		return fmt.Errorf("opensandbox: execd client not initialized")
	}
	return s.execd.UploadFile(ctx, file, opts)
}

// UploadFiles uploads one or more files to the sandbox in a single multipart request.
func (s *Sandbox) UploadFiles(ctx context.Context, entries []UploadFileEntry) error {
	if s.execd == nil {
		return fmt.Errorf("opensandbox: execd client not initialized")
	}
	return s.execd.UploadFiles(ctx, entries)
}

// DownloadFile downloads a file from the sandbox.
func (s *Sandbox) DownloadFile(ctx context.Context, remotePath, rangeHeader string, opts ...DownloadFileOptions) (io.ReadCloser, error) {
	if s.execd == nil {
		return nil, fmt.Errorf("opensandbox: execd client not initialized")
	}
	return s.execd.DownloadFile(ctx, remotePath, rangeHeader, opts...)
}

// CreateDirectory creates a directory in the sandbox.
// Mode is octal digits as int (e.g. 755 for rwxr-xr-x).
func (s *Sandbox) CreateDirectory(ctx context.Context, path string, mode int) error {
	if s.execd == nil {
		return fmt.Errorf("opensandbox: execd client not initialized")
	}
	return s.execd.CreateDirectory(ctx, path, mode)
}

// DeleteDirectory deletes a directory and its contents.
func (s *Sandbox) DeleteDirectory(ctx context.Context, path string) error {
	if s.execd == nil {
		return fmt.Errorf("opensandbox: execd client not initialized")
	}
	return s.execd.DeleteDirectory(ctx, path)
}

// ReplaceInFiles performs text replacement in files.
func (s *Sandbox) ReplaceInFiles(ctx context.Context, req ReplaceRequest) error {
	if s.execd == nil {
		return fmt.Errorf("opensandbox: execd client not initialized")
	}
	return s.execd.ReplaceInFiles(ctx, req)
}

// ReplaceInFilesDetailed performs text replacement and returns per-file replacement counts.
func (s *Sandbox) ReplaceInFilesDetailed(ctx context.Context, req ReplaceRequest) (ReplaceResponse, error) {
	if s.execd == nil {
		return nil, fmt.Errorf("opensandbox: execd client not initialized")
	}
	return s.execd.ReplaceInFilesDetailed(ctx, req)
}
