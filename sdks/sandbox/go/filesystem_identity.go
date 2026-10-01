// Copyright 2026 The OpenSandbox Authors
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
	"strings"
)

// IdentityFilesystem exposes file operations bound to explicit Linux credentials.
// It shares the sandbox transport without changing its default filesystem client.
type IdentityFilesystem interface {
	GetFileInfo(context.Context, string) (map[string]FileInfo, error)
	DeleteFiles(context.Context, []string) error
	SetPermissions(context.Context, PermissionsRequest) error
	MoveFiles(context.Context, MoveRequest) error
	SearchFiles(context.Context, string, string) ([]FileInfo, error)
	ListDirectory(context.Context, string) ([]FileInfo, error)
	ListDirectoryWithDepth(context.Context, string, int) ([]FileInfo, error)
	ReplaceInFiles(context.Context, ReplaceRequest) error
	ReplaceInFilesDetailed(context.Context, ReplaceRequest) (ReplaceResponse, error)
	UploadFile(context.Context, io.Reader, UploadFileOptions) error
	UploadFiles(context.Context, []UploadFileEntry) error
	DownloadFile(context.Context, string, string, ...DownloadFileOptions) (io.ReadCloser, error)
	CreateDirectory(context.Context, string, int) error
	DeleteDirectory(context.Context, string) error
}

// FilesWithIdentity returns an independent filesystem client for uid/gid.
// Unsupported servers fail; requests never fall back to the default identity.
func (e *ExecdClient) FilesWithIdentity(uid, gid uint32) (IdentityFilesystem, error) {
	if uid > ^uint32(0)-1 {
		return nil, &InvalidArgumentError{Field: "uid", Message: "must be at most 4294967294"}
	}
	if gid > ^uint32(0)-1 {
		return nil, &InvalidArgumentError{Field: "gid", Message: "must be at most 4294967294"}
	}
	if e == nil || e.client == nil {
		return nil, fmt.Errorf("opensandbox: execd client not initialized")
	}
	source := e.client
	baseURL := source.baseURL
	if marker := strings.Index(baseURL, "/v1/filesystem/"); marker >= 0 {
		baseURL = baseURL[:marker]
	}
	scoped := source.cloneWithBaseURL(strings.TrimRight(baseURL, "/") + fmt.Sprintf("/v1/filesystem/%d/%d", uid, gid))
	return &ExecdClient{client: scoped}, nil
}

// FilesWithIdentity returns a filesystem client using explicit Linux credentials.
// Keep sandbox credentials in the trusted backend that selects these identities.
func (s *Sandbox) FilesWithIdentity(uid, gid uint32) (IdentityFilesystem, error) {
	return s.execd.FilesWithIdentity(uid, gid)
}
