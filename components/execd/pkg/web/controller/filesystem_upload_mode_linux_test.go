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

//go:build linux

package controller

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

func withUmask(t *testing.T, mask int) {
	t.Helper()
	old := syscall.Umask(mask)
	t.Cleanup(func() { syscall.Umask(old) })
}

func uploadMultipart(t *testing.T, target string, metadata string, payload []byte) {
	t.Helper()
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	metadataPart, err := writer.CreateFormFile("metadata", "metadata")
	require.NoError(t, err)
	_, err = metadataPart.Write([]byte(metadata))
	require.NoError(t, err)
	filePart, err := writer.CreateFormFile("file", filepath.Base(target))
	require.NoError(t, err)
	_, err = filePart.Write(payload)
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	ctrl, rec := newFilesystemController(t, http.MethodPost, "/files/upload", body.Bytes())
	ctrl.ctx.Request.Header.Set("Content-Type", writer.FormDataContentType())
	ctrl.UploadFile()
	require.Equal(t, http.StatusOK, rec.Code)
}

func TestWriteUploadStreamNewFileUsesCreatMask(t *testing.T) {
	withUmask(t, 0o022)
	dir := t.TempDir()
	target := filepath.Join(dir, "notes.txt")

	uerr := writeUploadStream(target, bytes.NewReader([]byte("hello")))

	require.Nil(t, uerr)
	info, err := os.Stat(target)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o644), info.Mode().Perm())
}

func TestUploadFileWithoutModeUsesCreatMask(t *testing.T) {
	withUmask(t, 0o022)
	dir := t.TempDir()
	target := filepath.Join(dir, "notes.txt")

	uploadMultipart(t, target, `{"path":"`+filepath.ToSlash(target)+`"}`, []byte("hello"))

	info, err := os.Stat(target)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o644), info.Mode().Perm())
	requireNoUploadStaging(t, dir)
}

func TestUploadFileExplicitModeOverridesCreatMask(t *testing.T) {
	withUmask(t, 0o022)
	dir := t.TempDir()
	target := filepath.Join(dir, "run.sh")

	uploadMultipart(t, target, `{"path":"`+filepath.ToSlash(target)+`","mode":755}`, []byte("#!/bin/sh\n"))

	info, err := os.Stat(target)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o755), info.Mode().Perm())
}

func TestUploadFileWithoutModeKeepsExistingMode(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "helper")
	require.NoError(t, os.WriteFile(target, []byte("old"), 0o750))

	uploadMultipart(t, target, `{"path":"`+filepath.ToSlash(target)+`"}`, []byte("new"))

	got, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, "new", string(got))
	info, err := os.Stat(target)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o750), info.Mode().Perm())
}
