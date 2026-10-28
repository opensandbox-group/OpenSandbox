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

package controller

import (
	crand "crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/alibaba/opensandbox/execd/pkg/log"
	"github.com/alibaba/opensandbox/execd/pkg/util/pathutil"
	"github.com/alibaba/opensandbox/execd/pkg/web/model"
)

type uploadError struct {
	status  int
	code    model.ErrorCode
	message string
}

func newUploadError(status int, code model.ErrorCode, message string) *uploadError {
	return &uploadError{status: status, code: code, message: message}
}

func (c *FilesystemController) UploadFile() {
	rec := beginFilesystemMetric("upload")
	defer rec.Finish(c.basicController)

	metadataParts, fileParts, uerr := c.parseUploadForm()
	if uerr != nil {
		c.RespondError(uerr.status, uerr.code, uerr.message)
		return
	}

	for i := range metadataParts {
		if uerr := c.processUploadPair(metadataParts[i], fileParts[i]); uerr != nil {
			c.RespondError(uerr.status, uerr.code, uerr.message)
			return
		}
	}

	rec.MarkSuccess()
	c.RespondSuccess(nil)
}

func (c *FilesystemController) parseUploadForm() ([]*multipart.FileHeader, []*multipart.FileHeader, *uploadError) {
	return parseUploadForm(c.ctx)
}

func parseUploadForm(ctx *gin.Context) ([]*multipart.FileHeader, []*multipart.FileHeader, *uploadError) {
	form, err := ctx.MultipartForm()
	if err != nil || form == nil {
		return nil, nil, newUploadError(http.StatusBadRequest, model.ErrorCodeInvalidFile, "multipart form is empty")
	}

	metadataParts := form.File["metadata"]
	fileParts := form.File["file"]

	if len(metadataParts) == 0 {
		return nil, nil, newUploadError(http.StatusBadRequest, model.ErrorCodeInvalidFileMetadata, "metadata file is missing")
	}
	if len(fileParts) == 0 {
		return nil, nil, newUploadError(http.StatusBadRequest, model.ErrorCodeInvalidFileContent, "file is missing")
	}
	if len(metadataParts) != len(fileParts) {
		return nil, nil, newUploadError(
			http.StatusBadRequest,
			model.ErrorCodeInvalidFile,
			fmt.Sprintf("metadata and file count mismatch: %d vs %d", len(metadataParts), len(fileParts)),
		)
	}
	return metadataParts, fileParts, nil
}

func (c *FilesystemController) processUploadPair(metadataHeader, fileHeader *multipart.FileHeader) *uploadError {
	meta, uerr := parseUploadMetadata(metadataHeader)
	if uerr != nil {
		return uerr
	}

	resolvedPath, uerr := resolveUploadTarget(meta.Path, meta.Permission)
	if uerr != nil {
		return uerr
	}

	if uerr := writeUploadFile(resolvedPath, fileHeader); uerr != nil {
		return uerr
	}

	return applyUploadPermission(resolvedPath, meta.Permission)
}

func parseUploadMetadata(header *multipart.FileHeader) (*model.FileMetadata, *uploadError) {
	metadataFile, err := header.Open()
	if err != nil {
		return nil, newUploadError(
			http.StatusBadRequest,
			model.ErrorCodeInvalidFileMetadata,
			fmt.Sprintf("error opening metadata file. %v", err),
		)
	}
	metaBytes, err := io.ReadAll(metadataFile)
	metadataFile.Close()
	if err != nil {
		return nil, newUploadError(
			http.StatusBadRequest,
			model.ErrorCodeInvalidFileMetadata,
			fmt.Sprintf("error reading metadata content. %v", err),
		)
	}

	var meta model.FileMetadata
	if err := json.Unmarshal(metaBytes, &meta); err != nil {
		return nil, newUploadError(
			http.StatusBadRequest,
			model.ErrorCodeInvalidFileMetadata,
			fmt.Sprintf("invalid metadata format. %v", err),
		)
	}
	if meta.Path == "" {
		return nil, newUploadError(http.StatusBadRequest, model.ErrorCodeInvalidFileMetadata, "metadata path is empty")
	}
	return &meta, nil
}

func resolveUploadTarget(targetPath string, perm model.Permission) (string, *uploadError) {
	resolvedPath, err := pathutil.ExpandPath(targetPath)
	if err != nil {
		return "", newUploadError(
			http.StatusInternalServerError,
			model.ErrorCodeRuntimeError,
			fmt.Sprintf("error resolving target path %s. %v", targetPath, err),
		)
	}
	targetDir := filepath.Dir(resolvedPath)
	if err := MkdirAllWithOwnership(targetDir, os.ModePerm, perm.Owner, perm.Group); err != nil {
		return "", newUploadError(
			http.StatusInternalServerError,
			model.ErrorCodeRuntimeError,
			fmt.Sprintf("error creating target directory %s. %v", targetDir, err),
		)
	}
	return resolvedPath, nil
}

func writeUploadFile(resolvedPath string, fileHeader *multipart.FileHeader) *uploadError {
	file, err := fileHeader.Open()
	if err != nil {
		return newUploadError(
			http.StatusInternalServerError,
			model.ErrorCodeRuntimeError,
			fmt.Sprintf("error opening file %s. %v", fileHeader.Filename, err),
		)
	}
	defer file.Close()

	return writeUploadStream(resolvedPath, file)
}

// writeUploadStream stores an upload atomically: the bytes are staged in a
// temp file inside the destination directory and renamed over the destination
// only after every byte is written and synced. Any failure before that point -
// a broken request stream, ENOSPC, an I/O error, execd dying mid-copy - leaves
// the destination exactly as it was. The previous open(O_TRUNC)-then-copy
// design truncated the destination up front, turning a failed upload into
// unrecoverable data loss (#2432).
func writeUploadStream(resolvedPath string, src io.Reader) *uploadError {
	dest := followUploadSymlink(resolvedPath)

	// Classify the destination once. A directory keeps being refused, and a
	// special file (device, FIFO, socket) was never replaceable by a rename
	// and was always streamed into in place, so it keeps that path.
	destInfo, statErr := os.Lstat(dest)
	existing := statErr == nil
	if existing {
		if destInfo.IsDir() {
			return newUploadError(
				http.StatusInternalServerError,
				model.ErrorCodeRuntimeError,
				fmt.Sprintf("destination %s is a directory", resolvedPath),
			)
		}
		if !destInfo.Mode().IsRegular() {
			return copyInPlace(dest, src, resolvedPath, os.ModePerm)
		}
	}

	// Reject a destination this process cannot open for writing before
	// staging anything: a rename would otherwise replace a file the old code
	// could not even open. The probe carries no O_TRUNC, so it cannot damage
	// the destination.
	if existing {
		probe, err := os.OpenFile(dest, os.O_WRONLY, 0)
		if err != nil {
			return newUploadError(
				http.StatusInternalServerError,
				model.ErrorCodeRuntimeError,
				fmt.Sprintf("error opening destination file %s. %v", resolvedPath, err),
			)
		}
		probe.Close()
	}

	// Keep what an in-place write preserved for free: the destination's
	// mode, and on Unix its owner and group.
	mode := os.ModePerm
	uid, gid := -1, -1
	if existing {
		mode = destInfo.Mode().Perm()
		uid, gid = uploadOwnerOf(destInfo)
	}

	targetDir := filepath.Dir(dest)

	// The staging file's creation mode is what the kernel umask shapes, so
	// a new destination gets exactly the bits the old O_CREATE produced.
	// Replacing a destination stages 0600 and chmods the exact previous
	// mode on top afterwards, because chmod is not umask-filtered.
	stagedMode := os.ModePerm
	if existing {
		stagedMode = 0o600
	}
	staged, err := openStaging(targetDir, stagedMode)
	if err != nil {
		if existing && errors.Is(err, os.ErrPermission) {
			// The parent directory is not writable, but the destination
			// itself is (the probe above passed) - a 0666-file-under-0555
			// layout the in-place write always served. No atomic path
			// exists there, so keep serving it with the historical
			// in-place write instead of failing the upload.
			return copyInPlace(dest, src, resolvedPath, mode)
		}
		return newUploadError(
			http.StatusInternalServerError,
			model.ErrorCodeRuntimeError,
			fmt.Sprintf("error creating staging file for %s. %v", resolvedPath, err),
		)
	}
	stagedName := staged.Name()
	keepStaging := false
	defer func() {
		if !keepStaging {
			os.Remove(stagedName)
		}
	}()

	if existing {
		if err := os.Chmod(stagedName, mode); err != nil {
			staged.Close()
			return newUploadError(
				http.StatusInternalServerError,
				model.ErrorCodeRuntimeError,
				fmt.Sprintf("error chmoding staging file for %s. %v", resolvedPath, err),
			)
		}
	}

	if _, err := io.Copy(staged, src); err != nil {
		staged.Close()
		return newUploadError(
			http.StatusInternalServerError,
			model.ErrorCodeRuntimeError,
			fmt.Sprintf("error copying file %s. %v", resolvedPath, err),
		)
	}
	if err := staged.Sync(); err != nil {
		staged.Close()
		return newUploadError(
			http.StatusInternalServerError,
			model.ErrorCodeRuntimeError,
			fmt.Sprintf("error syncing staging file for %s. %v", resolvedPath, err),
		)
	}
	if err := staged.Close(); err != nil {
		return newUploadError(
			http.StatusInternalServerError,
			model.ErrorCodeRuntimeError,
			fmt.Sprintf("error closing staging file for %s. %v", resolvedPath, err),
		)
	}

	if existing && (uid >= 0 || gid >= 0) {
		if err := os.Chown(stagedName, uid, gid); err != nil {
			if !errors.Is(err, os.ErrPermission) {
				return newUploadError(
					http.StatusInternalServerError,
					model.ErrorCodeRuntimeError,
					fmt.Sprintf("error chowning staging file for %s. %v", resolvedPath, err),
				)
			}
			// Non-root execd cannot re-own the staging file to the
			// destination's owner, and a rename would silently hand the
			// destination to execd's own user - ownership the in-place
			// write preserved for free. The staged bytes are complete and
			// synced by now, so write them into the destination in place
			// instead; if even that fails, the staging file is kept and
			// named in the error so the upload's contents survive.
			keepStaging = true
			return replaceFromStaging(stagedName, dest, resolvedPath, &keepStaging)
		}
	}

	if err := publishStagedUpload(stagedName, dest, resolvedPath, destInfo, existing, &keepStaging); err != nil {
		return err
	}

	// fsync parent directory so the publish is durable and visible on
	// weakly-coherent filesystems (virtio-fs, 9pfs, etc.). Best-effort:
	// some filesystems return ENOTSUP for directory fsync.
	if d, err := os.Open(targetDir); err == nil {
		if err := d.Sync(); err != nil {
			log.Warn("upload: sync parent dir %s: %v", targetDir, err)
		}
		_ = d.Close()
	}
	return nil
}

// renameUpload exists so tests can inject the rename failures real
// destinations produce: a writable single-file bind mount answers EBUSY/EXDEV
// to rename(2) while the old in-place write served it.
var renameUpload = os.Rename

// publishStagedUpload makes a fully written, synced staging file the
// destination. It prefers rename(2), which is atomic and never leaves the
// destination partial, and routes to an in-place publish from the staging
// file wherever a rename would change what the destination *is* - the cases
// an inode swap cannot reproduce:
//
//   - hard links: a rename repoints one directory entry, so every other name
//     of the inode would keep the old bytes, which the old O_TRUNC write kept
//     in sync;
//   - extended attributes, including the POSIX access ACL: a new inode starts
//     without them, which for an ACL can silently widen access. They are
//     copied onto the staging file first, and a set that the process cannot
//     reproduce (security.* needs privilege) falls back to in place;
//   - destinations rename(2) refuses to replace at all: a writable
//     single-file bind mount is a mount point and answers EBUSY/EXDEV.
func publishStagedUpload(
	stagedName, dest, resolvedPath string,
	destInfo os.FileInfo,
	existing bool,
	keepStaging *bool,
) *uploadError {
	if existing {
		if uploadLinkCount(destInfo) > 1 {
			return replaceFromStaging(stagedName, dest, resolvedPath, keepStaging)
		}
		if !copyUploadAttrs(dest, stagedName) {
			return replaceFromStaging(stagedName, dest, resolvedPath, keepStaging)
		}
	}

	if err := renameUpload(stagedName, dest); err != nil {
		return replaceFromStaging(stagedName, dest, resolvedPath, keepStaging)
	}
	return nil
}

// openStaging creates a unique dot-prefixed staging file in dir with the
// given creation mode, so the process umask applies to it exactly as it did
// to the destination file the old O_CREATE path produced.
func openStaging(dir string, mode os.FileMode) (*os.File, error) {
	var seed [4]byte
	for i := 0; i < 10000; i++ {
		if _, err := crand.Read(seed[:]); err != nil {
			return nil, err
		}
		name := filepath.Join(dir, fmt.Sprintf(".upload-%08x.tmp", binary.BigEndian.Uint32(seed[:])))
		f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		return f, err
	}
	return nil, os.ErrExist
}

// copyInPlace is the historical write algorithm, used only where publishing a
// new inode would change what the destination is: special files, hard-linked
// aliases, attributes the staging file cannot carry, a writable destination
// under an unwritable parent directory, a foreign-owned destination a non-root
// execd cannot re-own its staging file to, and destinations rename(2) refuses
// (mount points such as single-file bind mounts). Those corners always wrote
// in place, and failing uploads that always worked would be a regression.
func copyInPlace(dest string, src io.Reader, resolvedPath string, createMode os.FileMode) *uploadError {
	dst, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, createMode)
	if err != nil {
		return newUploadError(
			http.StatusInternalServerError,
			model.ErrorCodeRuntimeError,
			fmt.Sprintf("error opening destination file %s. %v", resolvedPath, err),
		)
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		return newUploadError(
			http.StatusInternalServerError,
			model.ErrorCodeRuntimeError,
			fmt.Sprintf("error copying file %s. %v", resolvedPath, err),
		)
	}
	if err := dst.Sync(); err != nil {
		// A device or FIFO answers EINVAL/ENOTSUP to fsync, and the
		// historical write only logged that, so a special destination keeps
		// working; a regular file still reports the failure.
		if fi, statErr := dst.Stat(); statErr == nil && fi.Mode().IsRegular() {
			dst.Close()
			return newUploadError(
				http.StatusInternalServerError,
				model.ErrorCodeRuntimeError,
				fmt.Sprintf("error syncing destination file %s. %v", resolvedPath, err),
			)
		}
		log.Warn("upload: sync destination %s: %v", resolvedPath, err)
	}
	if err := dst.Close(); err != nil {
		return newUploadError(
			http.StatusInternalServerError,
			model.ErrorCodeRuntimeError,
			fmt.Sprintf("error closing destination file %s. %v", resolvedPath, err),
		)
	}
	return nil
}

// replaceFromStaging streams a fully staged upload into the existing
// destination in place (see copyInPlace). On failure the staging file is
// kept and its path is surfaced in the error, so the complete uploaded
// contents survive for manual recovery instead of being deleted.
func replaceFromStaging(stagedName, dest, resolvedPath string, keepStaging *bool) *uploadError {
	f, err := os.Open(stagedName)
	if err != nil {
		return newUploadError(
			http.StatusInternalServerError,
			model.ErrorCodeRuntimeError,
			fmt.Sprintf("error reopening staging file %s. %v", stagedName, err),
		)
	}
	defer f.Close()

	if uerr := copyInPlace(dest, f, resolvedPath, os.ModePerm); uerr != nil {
		return newUploadError(
			http.StatusInternalServerError,
			model.ErrorCodeRuntimeError,
			fmt.Sprintf(
				"error replacing file %s in place: %v; the complete uploaded contents are preserved at %s",
				resolvedPath, uerr.message, stagedName,
			),
		)
	}
	*keepStaging = false
	return nil
}

// followUploadSymlink resolves a symlink in the final path component the way
// the previous in-place open did: an upload through `link -> real` must
// replace `real`, not the link itself, and a dangling link must create its
// (still missing) target rather than overwriting the link. A relative link
// target is joined onto the physical directory that contains the link -
// resolved with EvalSymlinks - never onto the lexical one, so `..` segments
// and symlinked parents resolve the way the kernel would have.
// Symlinks in parent components of the requested path itself are followed by
// the kernel for open and rename alike, so only the final component is
// walked here.
func followUploadSymlink(path string) string {
	for i := 0; i < 40; i++ {
		fi, err := os.Lstat(path)
		if err != nil || fi.Mode()&os.ModeSymlink == 0 {
			return path
		}
		dest, err := os.Readlink(path)
		if err != nil {
			return path
		}
		dir, err := filepath.EvalSymlinks(filepath.Dir(path))
		if err != nil {
			dir = filepath.Dir(path)
		}
		if !filepath.IsAbs(dest) {
			dest = filepath.Join(dir, dest)
		}
		path = dest
	}
	return path
}

// applyUploadPermission applies the metadata permission with one retry to
// absorb metadata-propagation delay on weakly-coherent filesystems
// (virtio-fs, 9pfs). ChmodFile always invokes chown under the hood, so a
// freshly-created dirent that has not yet propagated will surface as ENOENT
// here even though the file is fully written and synced.
func applyUploadPermission(resolvedPath string, permission model.Permission) *uploadError {
	chmodErr := ChmodFile(resolvedPath, permission)
	if chmodErr != nil {
		time.Sleep(20 * time.Millisecond)
		chmodErr = ChmodFile(resolvedPath, permission)
	}
	if chmodErr != nil {
		return newUploadError(
			http.StatusInternalServerError,
			model.ErrorCodeRuntimeError,
			fmt.Sprintf("error chmoding file %s. %v", resolvedPath, chmodErr),
		)
	}
	return nil
}
