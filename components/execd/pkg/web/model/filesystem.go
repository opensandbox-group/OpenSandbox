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

package model

import "time"

// FileInfo represents file metadata including path and permissions
type FileInfo struct {
	Path       string    `json:"path,omitempty"`
	Size       int64     `json:"size"`
	ModifiedAt time.Time `json:"modified_at,omitempty"`
	CreatedAt  time.Time `json:"created_at,omitempty"`
	Permission `json:",inline"`
}

type FileMetadata struct {
	Path       string `json:"path,omitempty"`
	Permission `json:",inline"`
}

// Permission represents file ownership and mode
type Permission struct {
	Owner string `json:"owner"`
	Group string `json:"group"`
	Mode  int    `json:"mode"`
}

// RenameFileItem represents a file rename operation
type RenameFileItem struct {
	Src  string `json:"src,omitempty"`
	Dest string `json:"dest,omitempty"`
}

// ReplaceFileContentItem represents a content replacement operation
type ReplaceFileContentItem struct {
	Old string `json:"old,omitempty"`
	New string `json:"new,omitempty"`
}

// ReplaceFileContentResult represents the result of a content replacement on a single file
type ReplaceFileContentResult struct {
	ReplacedCount int `json:"replacedCount"`
}
