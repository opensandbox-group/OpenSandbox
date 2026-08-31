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

package file

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"hash/crc64"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/alibaba/opensandbox/nodeagent/pkg/api"
	"github.com/alibaba/opensandbox/nodeagent/pkg/config"
	"github.com/alibaba/opensandbox/nodeagent/pkg/identity"
	"github.com/alibaba/opensandbox/nodeagent/pkg/marker"
	"github.com/alibaba/opensandbox/nodeagent/pkg/objectlayout"
	"github.com/alibaba/opensandbox/nodeagent/pkg/registry"
	"github.com/alibaba/opensandbox/nodeagent/pkg/state"
	"github.com/alibaba/opensandbox/nodeagent/pkg/streamformat"
)

const name = "file"

func init() {
	registry.RegisterSink(name, func(cfg config.Config) (string, error) {
		if cfg.FilePath == "" {
			return identity.StdoutTargetID(cfg.ClusterID, cfg.NodeName), nil
		}
		return identity.FileTargetID(cfg.FilePath, cfg.ClusterID, cfg.NodeName)
	}, func(dependencies registry.SinkDependencies) (api.Sink, error) {
		cfg := dependencies.Config
		return newFileSink(fileConfig{Root: cfg.FilePath, ClusterID: cfg.ClusterID, MaxFileBytes: cfg.FileMaxBytes, MaxFiles: cfg.FileMaxFiles, MaxTotalBytes: cfg.FileMaxTotalBytes, Retention: cfg.FileRetention}, dependencies.State)
	})
}

type stateStore interface {
	GetSinkStream(sinkName, streamRef string) (state.SinkStream, bool, error)
	PutSinkStream(sinkName string, stream state.SinkStream) error
	ListSourceStreams() ([]state.SourceStream, error)
	DeleteStream(streamRef string) error
}

type fileConfig struct {
	Root          string
	ClusterID     string
	MaxFileBytes  int64
	MaxFiles      int
	MaxTotalBytes int64
	Retention     time.Duration
}

type fileSink struct {
	cfg   fileConfig
	state stateStore

	mu            sync.Mutex
	writers       map[string]*writer
	capacityUsed  int64
	capacityKnown bool
}

type writer struct {
	stream   state.SinkStream
	kind     api.RecordKind
	resource api.Resource
	metadata api.StreamMetadata
	family   objectlayout.Family
	file     *os.File
	crc      hash.Hash64
}

type capacityExhaustedError struct {
	limit int64
}

func (e capacityExhaustedError) Error() string {
	return fmt.Sprintf("durable file total-byte limit %d would be exceeded", e.limit)
}
func (capacityExhaustedError) Retryable() bool { return true }

func newFileSink(cfg fileConfig, store stateStore) (*fileSink, error) {
	if cfg.MaxFileBytes <= 0 || cfg.MaxFiles <= 0 || (cfg.Root != "" && cfg.MaxTotalBytes <= 0) {
		return nil, errors.New("file limits must be positive")
	}
	if cfg.Root != "" {
		canonical, err := filepath.Abs(filepath.Clean(cfg.Root))
		if err != nil {
			return nil, err
		}
		if resolved, resolveErr := filepath.EvalSymlinks(canonical); resolveErr == nil {
			canonical = resolved
		} else if !errors.Is(resolveErr, os.ErrNotExist) {
			return nil, resolveErr
		}
		cfg.Root = canonical
		if err := mkdirAllNoFollow(cfg.Root, 0o750); err != nil {
			return nil, err
		}
	}
	return &fileSink{cfg: cfg, state: store, writers: make(map[string]*writer)}, nil
}

func (s *fileSink) Capabilities() api.Capabilities {
	return api.Capabilities{RecordKinds: streamformat.Kinds()}
}
func (s *fileSink) Guarantee() api.DeliveryGuarantee {
	if s.cfg.Root == "" {
		return api.GuaranteeBestEffort
	}
	return api.GuaranteeDurable
}

func (s *fileSink) Consume(_ context.Context, batch api.Batch) error {
	if len(batch.Items) == 0 {
		return nil
	}
	format, resource, data, err := streamformat.EncodeBatch(batch)
	if err != nil {
		return api.Permanent(fmt.Errorf("encode file batch: %w", err))
	}
	if s.cfg.Root == "" {
		s.mu.Lock()
		defer s.mu.Unlock()
		_, err := os.Stdout.Write(data)
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if resource.ClusterName != s.cfg.ClusterID {
		return api.Permanent(fmt.Errorf("resource cluster %q does not match configured cluster %q", resource.ClusterName, s.cfg.ClusterID))
	}
	if int64(len(data)) > s.cfg.MaxFileBytes {
		return api.Permanent(fmt.Errorf("encoded batch size %d exceeds per-generation limit %d", len(data), s.cfg.MaxFileBytes))
	}
	digest := sha256.Sum256(data)
	w, err := s.getWriter(batch.StreamRef, resource, batch.Metadata, format)
	if err != nil {
		return err
	}
	if w.stream.AppendIntent != nil {
		intent := fileAppendIntent(w.stream, int64(len(data)), digest)
		if *w.stream.AppendIntent != intent {
			return api.Permanent(errors.New("file append retry does not match persisted intent"))
		}
		if err := recoverAppend(w); err != nil {
			return err
		}
	}
	requiresNextGeneration := w.stream.CurrentClosed ||
		(w.stream.Position > 0 && w.stream.Position > s.cfg.MaxFileBytes-int64(len(data)))
	if requiresNextGeneration && (s.cfg.MaxFiles <= 0 || w.stream.Generation >= uint64(s.cfg.MaxFiles-1)) {
		return api.Permanent(errors.New("durable file generation limit reached"))
	}
	if err := s.reserveCapacity(int64(len(data))); err != nil {
		return err
	}
	if w.stream.CurrentClosed {
		if err := s.startNextGeneration(w); err != nil {
			return err
		}
	} else if requiresNextGeneration {
		if err := s.rollover(w); err != nil {
			return err
		}
	}
	intent := fileAppendIntent(w.stream, int64(len(data)), digest)
	if w.stream.AppendIntent == nil {
		w.stream.AppendIntent = &intent
		if err := s.state.PutSinkStream(name, w.stream); err != nil {
			w.stream.AppendIntent = nil
			return err
		}
	} else {
		if *w.stream.AppendIntent != intent {
			return api.Permanent(errors.New("file append retry does not match persisted intent"))
		}
	}
	if err := writeFull(w.file, data); err != nil {
		return s.recoverFailedAppend(w, err)
	}
	if err := syncData(w.file); err != nil {
		return s.recoverFailedAppend(w, err)
	}
	_, _ = w.crc.Write(data)
	next := w.stream
	next.Position += int64(len(data))
	next.CRC64State, err = w.crc.(encoding.BinaryMarshaler).MarshalBinary()
	if err != nil {
		return s.recoverFailedAppend(w, err)
	}
	next.AppendIntent = nil
	if err := s.state.PutSinkStream(name, next); err != nil {
		return s.recoverFailedAppend(w, err)
	}
	w.stream = next
	s.adjustCapacity(int64(len(data)))
	return nil
}

func (s *fileSink) Finalize(ctx context.Context, request api.FinalizeRequest) error {
	format, err := streamformat.Lookup(request.StreamRef.Kind)
	if err != nil {
		return api.Permanent(fmt.Errorf("finalize file stream: %w", err))
	}
	if s.cfg.Root == "" {
		return nil
	}
	if err := request.Metadata.Validate(); err != nil {
		return api.Permanent(fmt.Errorf("finalize file stream: %w", err))
	}
	family, err := streamformat.ResolveFamily(format, "", request.StreamRef, request.Resource, request.Metadata)
	if err != nil {
		return api.Permanent(fmt.Errorf("resolve file object family: %w", err))
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if request.Resource.ClusterName != s.cfg.ClusterID {
		return api.Permanent(fmt.Errorf("resource cluster %q does not match configured cluster %q", request.Resource.ClusterName, s.cfg.ClusterID))
	}
	w := s.writers[request.StreamRef.ID]
	var stream state.SinkStream
	if w == nil {
		var found bool
		var err error
		stream, found, err = s.state.GetSinkStream(name, request.StreamRef.ID)
		if err != nil {
			return err
		}
		if !found {
			stream = state.SinkStream{SinkName: name, StreamRef: request.StreamRef.ID}
		} else if !stream.CurrentClosed {
			w, err = s.getWriter(request.StreamRef, request.Resource, request.Metadata, format)
			if err != nil {
				return err
			}
		}
	}
	if w != nil {
		if w.kind != request.StreamRef.Kind {
			return api.Permanent(errors.New("file finalization record kind changed"))
		}
		if w.resource != request.Resource || !w.metadata.Equal(request.Metadata) {
			return api.Permanent(errors.New("file finalization resource identity changed"))
		}
		if err := s.closeGeneration(w); err != nil {
			return err
		}
		stream = w.stream
	}
	if request.Revision < stream.FinalizedRevision || request.Revision > stream.FinalizedRevision+1 {
		return api.Permanent(fmt.Errorf("file marker revision %d is not continuous after %d", request.Revision, stream.FinalizedRevision))
	}
	if err := s.verifyClosedFiles(ctx, request.Resource, request.Metadata, stream, format); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	raw, err := marker.Encode(marker.New(request, stream.ClosedObjects))
	if err != nil {
		return api.Permanent(err)
	}
	dir, err := familyPath(s.cfg.Root, family)
	if err != nil {
		return err
	}
	if err := mkdirAllNoFollow(dir, 0o750); err != nil {
		return err
	}
	markerPath, err := safeObjectPath(s.cfg.Root, family.MarkerKey(request.Revision))
	if err != nil {
		return err
	}
	digest := sha256.Sum256(raw)
	markerStem := strings.TrimSuffix(family.MarkerName(request.Revision), ".json")
	tmpName := filepath.Join(dir, fmt.Sprintf(".%s.%s.tmp", markerStem, hex.EncodeToString(digest[:8])))
	if existing, readErr := readNoFollow(markerPath); readErr == nil {
		if !bytes.Equal(existing, raw) {
			return api.Permanent(errors.New("conflicting finalization marker already exists"))
		}
		if err := syncDir(dir); err != nil {
			return err
		}
		_, _ = removeTemporaryMarker(tmpName)
		s.invalidateCapacity()
		stream.FinalizedRevision = request.Revision
		stream.MarkerIntent = nil
		if err := s.state.PutSinkStream(name, stream); err != nil {
			return err
		}
		if w != nil {
			w.stream = stream
		}
		return nil
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return readErr
	}
	intent := state.MarkerIntent{Revision: request.Revision, Path: markerPath, TempPath: tmpName, SHA256: hex.EncodeToString(digest[:])}
	if stream.MarkerIntent != nil && *stream.MarkerIntent != intent {
		return api.Permanent(errors.New("file marker intent conflicts with finalization request"))
	}
	temporaryExists := false
	if existingFile, openErr := openNoFollowExisting(tmpName); openErr == nil {
		existing, readErr := io.ReadAll(existingFile)
		if readErr != nil {
			_ = existingFile.Close()
			return readErr
		}
		if !bytes.Equal(existing, raw) {
			if stream.MarkerIntent == nil || len(existing) >= len(raw) || !bytes.Equal(existing, raw[:len(existing)]) {
				_ = existingFile.Close()
				return api.Permanent(errors.New("temporary marker bytes conflict with marker intent"))
			}
			additional := int64(len(raw) - len(existing))
			if err := s.reserveCapacity(additional); err != nil {
				_ = existingFile.Close()
				return err
			}
			if err := existingFile.Truncate(0); err != nil {
				_ = existingFile.Close()
				s.invalidateCapacity()
				return classifyPathError("truncate temporary finalization marker", tmpName, err)
			}
			if _, err := existingFile.Seek(0, io.SeekStart); err != nil {
				_ = existingFile.Close()
				s.invalidateCapacity()
				return err
			}
			if err := writeFull(existingFile, raw); err != nil {
				_ = existingFile.Close()
				s.invalidateCapacity()
				return err
			}
			if err := existingFile.Sync(); err != nil {
				_ = existingFile.Close()
				s.invalidateCapacity()
				return classifyPathError("sync temporary finalization marker", tmpName, err)
			}
			if err := existingFile.Close(); err != nil {
				s.invalidateCapacity()
				return err
			}
			s.adjustCapacity(additional)
			temporaryExists = true
		} else {
			if err := existingFile.Sync(); err != nil {
				_ = existingFile.Close()
				return classifyPathError("sync temporary finalization marker", tmpName, err)
			}
			if err := existingFile.Close(); err != nil {
				return err
			}
			temporaryExists = true
		}
	} else if !errors.Is(openErr, os.ErrNotExist) {
		return openErr
	}
	if !temporaryExists {
		if err := s.reserveCapacity(int64(len(raw))); err != nil {
			return err
		}
	}
	if stream.MarkerIntent == nil {
		stream.MarkerIntent = &intent
		if err := s.state.PutSinkStream(name, stream); err != nil {
			return err
		}
	}
	if !temporaryExists {
		tmp, err := createNoFollowExclusive(tmpName)
		if err != nil {
			return classifyPathError("create temporary finalization marker", tmpName, err)
		}
		if err := writeFull(tmp, raw); err != nil {
			_ = tmp.Close()
			s.invalidateCapacity()
			return err
		}
		if err := tmp.Sync(); err != nil {
			_ = tmp.Close()
			s.invalidateCapacity()
			return classifyPathError("sync temporary finalization marker", tmpName, err)
		}
		if err := tmp.Close(); err != nil {
			s.invalidateCapacity()
			return err
		}
		s.adjustCapacity(int64(len(raw)))
	}
	if err := syncDir(dir); err != nil {
		return err
	}
	capacityUncertain, err := publishMarker(tmpName, markerPath, raw)
	if capacityUncertain || err != nil {
		s.invalidateCapacity()
	}
	if err != nil {
		return err
	}
	stream.FinalizedRevision = request.Revision
	stream.MarkerIntent = nil
	if err := s.state.PutSinkStream(name, stream); err != nil {
		return err
	}
	if w != nil {
		w.stream = stream
	}
	return nil
}

func publishMarker(temporaryPath, markerPath string, raw []byte) (bool, error) {
	dir := filepath.Dir(markerPath)
	if err := renameNoReplace(temporaryPath, markerPath); err != nil {
		existing, readErr := readNoFollow(markerPath)
		if readErr != nil {
			return false, errors.Join(err, readErr)
		}
		if !bytes.Equal(existing, raw) {
			return false, api.Permanent(errors.Join(errors.New("conflicting finalization marker already exists"), err))
		}
		if syncErr := syncDir(dir); syncErr != nil {
			return true, errors.Join(err, syncErr)
		}
		_, _ = removeTemporaryMarker(temporaryPath)
		return true, nil
	}
	return false, syncDir(dir)
}

func removeTemporaryMarker(temporaryPath string) (bool, error) {
	dir := filepath.Dir(temporaryPath)
	if err := os.Remove(temporaryPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, classifyPathError("remove temporary finalization marker", temporaryPath, err)
	} else if errors.Is(err, os.ErrNotExist) {
		return false, syncDir(dir)
	}
	return true, syncDir(dir)
}

func (s *fileSink) Close(_ context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var errs []error
	for id, writer := range s.writers {
		if writer.file != nil {
			errs = append(errs, writer.file.Close())
		}
		delete(s.writers, id)
	}
	return errors.Join(errs...)
}

// CollectExpired removes only whole durable-file object families. The
// persisted cleanup phase is the tombstone: a crash resumes from either the
// canonical family path or the GC staging path and never deletes one
// generation in isolation.
func (s *fileSink) CollectExpired(ctx context.Context, now time.Time) error {
	if s.cfg.Root == "" {
		return nil
	}
	sources, err := s.state.ListSourceStreams()
	if err != nil {
		return err
	}
	var errs []error
	for _, source := range sources {
		if err := ctx.Err(); err != nil {
			return errors.Join(err, errors.Join(errs...))
		}
		if err := s.collectExpiredStream(ctx, now, source); err != nil {
			errs = append(errs, fmt.Errorf("collect expired stream %q: %w", source.StreamRef, err))
		}
	}
	return errors.Join(errs...)
}

func (s *fileSink) collectExpiredStream(ctx context.Context, now time.Time, source state.SourceStream) error {
	if !source.Ended || source.RepairDeadline == nil || now.Before(source.RepairDeadline.Add(s.cfg.Retention)) {
		return nil
	}
	if _, err := os.Stat(source.Resource.LogDirectory); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	stream, found, err := s.state.GetSinkStream(name, source.StreamRef)
	if err != nil {
		return err
	}
	if !found || stream.FinalizedRevision < source.AcknowledgedRevision {
		return nil
	}
	if writer := s.writers[source.StreamRef]; writer != nil && writer.file != nil {
		return nil
	}
	resource := api.Resource{SandboxID: source.Resource.SandboxID, ClusterName: source.Resource.ClusterName, Namespace: source.Resource.Namespace, PodName: source.Resource.PodName, PodUID: source.Resource.PodUID, NodeName: source.Resource.NodeName, Container: source.Resource.Container}
	format, err := streamformat.Lookup(api.RecordKindContainerLog)
	if err != nil {
		return err
	}
	metadata := api.StreamMetadata{streamformat.ContainerLogDirectoryMetadata: source.Resource.LogDirectory}
	streamRef := api.StreamRef{ID: source.StreamRef, Kind: api.RecordKindContainerLog}
	family, err := streamformat.ResolveFamily(format, "", streamRef, resource, metadata)
	if err != nil {
		return err
	}
	directory, err := familyPath(s.cfg.Root, family)
	if err != nil {
		return err
	}
	digest := sha256.Sum256([]byte(source.StreamRef))
	gcDir := filepath.Join(s.cfg.Root, ".gc")
	staging := filepath.Join(gcDir, hex.EncodeToString(digest[:]))
	if stream.CleanupPhase == "" {
		stream.CleanupPhase = "planned"
		stream.CleanupPath = staging
		if err := s.state.PutSinkStream(name, stream); err != nil {
			return err
		}
	}
	if stream.CleanupPath != staging {
		return api.Permanent(errors.New("durable-file cleanup staging path conflicts with checkpoint"))
	}
	if stream.CleanupPhase == "planned" {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := mkdirAllNoFollow(gcDir, 0o700); err != nil {
			return err
		}
		if _, err := os.Stat(staging); errors.Is(err, os.ErrNotExist) {
			if err := os.Rename(directory, staging); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		} else if err != nil {
			return err
		}
		if err := syncDir(filepath.Dir(directory)); err != nil {
			return err
		}
		if err := syncDir(gcDir); err != nil {
			return err
		}
		stream.CleanupPhase = "staged"
		if err := s.state.PutSinkStream(name, stream); err != nil {
			return err
		}
	}
	if stream.CleanupPhase == "staged" {
		if err := ctx.Err(); err != nil {
			return err
		}
		removeErr := os.RemoveAll(staging)
		s.invalidateCapacity()
		if removeErr != nil {
			return removeErr
		}
		if err := syncDir(gcDir); err != nil {
			return err
		}
		stream.CleanupPhase = "deleted"
		if err := s.state.PutSinkStream(name, stream); err != nil {
			return err
		}
	}
	if stream.CleanupPhase != "deleted" {
		return api.Permanent(fmt.Errorf("unknown durable-file cleanup phase %q", stream.CleanupPhase))
	}
	delete(s.writers, source.StreamRef)
	return s.state.DeleteStream(source.StreamRef)
}

func (s *fileSink) getWriter(streamRef api.StreamRef, resource api.Resource, metadata api.StreamMetadata, format streamformat.Format) (*writer, error) {
	family, err := streamformat.ResolveFamily(format, "", streamRef, resource, metadata)
	if err != nil {
		return nil, api.Permanent(fmt.Errorf("resolve file object family: %w", err))
	}
	if existing := s.writers[streamRef.ID]; existing != nil {
		if existing.kind != streamRef.Kind {
			return nil, api.Permanent(errors.New("file stream record kind changed"))
		}
		if existing.resource != resource || !existing.metadata.Equal(metadata) || existing.family != family {
			return nil, api.Permanent(errors.New("file stream resource identity changed"))
		}
		return existing, nil
	}
	stream, found, err := s.state.GetSinkStream(name, streamRef.ID)
	if err != nil {
		return nil, err
	}
	if !found {
		stream = state.SinkStream{SinkName: name, StreamRef: streamRef.ID}
	} else if stream.StreamRef != streamRef.ID {
		return nil, api.Permanent(errors.New("durable file checkpoint stream reference mismatch"))
	}
	dir, err := familyPath(s.cfg.Root, family)
	if err != nil {
		return nil, err
	}
	path, err := safeObjectPath(s.cfg.Root, family.DataKey(stream.Generation))
	if err != nil {
		return nil, err
	}
	expectedObjectKey := family.DataKey(stream.Generation)
	if found && stream.ObjectKey != "" && stream.ObjectKey != expectedObjectKey {
		return nil, api.Permanent(errors.New("durable file object key does not match checkpoint generation"))
	}
	if found && stream.ObjectKey == "" && (stream.Position != 0 || stream.CurrentClosed || len(stream.ClosedObjects) != 0) {
		return nil, api.Permanent(errors.New("durable file checkpoint is missing its object key"))
	}
	stream.ObjectKey = expectedObjectKey
	if found {
		if err := s.validateClosedFileLayout(resource, metadata, stream, format); err != nil {
			return nil, err
		}
	}
	if stream.CurrentClosed {
		if stream.AppendIntent != nil {
			return nil, api.Permanent(errors.New("closed durable-file generation has an unresolved append intent"))
		}
		w := &writer{stream: stream, kind: streamRef.Kind, resource: resource, metadata: metadata.Clone(), family: family, crc: crc64.New(crc64.MakeTable(crc64.ECMA))}
		s.writers[streamRef.ID] = w
		return w, nil
	}
	if err := mkdirAllNoFollow(dir, 0o750); err != nil {
		return nil, err
	}
	f, err := openNoFollow(path)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !found && info.Size() != 0 {
		_ = f.Close()
		if err := quarantineOrphan(s.cfg.Root, path); err != nil {
			return nil, fmt.Errorf("quarantine non-empty file %s: %w", path, err)
		}
		f, err = openNoFollow(path)
		if err != nil {
			return nil, err
		}
		info, err = f.Stat()
		if err != nil {
			_ = f.Close()
			return nil, err
		}
	}
	device, inode, err := fileIdentity(info)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if found && stream.Device != 0 && (stream.Device != device || stream.Inode != inode) {
		_ = f.Close()
		return nil, api.Permanent(errors.New("durable file identity does not match checkpoint"))
	}
	stream.Device = device
	stream.Inode = inode
	actualSize := info.Size()
	if found && stream.AppendIntent != nil {
		intent := stream.AppendIntent
		if intent.Device != 0 && (intent.Device != device || intent.Inode != inode) {
			_ = f.Close()
			return nil, api.Permanent(errors.New("file append intent identity mismatch"))
		}
		if actualSize < intent.Position || actualSize > intent.Position+intent.Length {
			_ = f.Close()
			return nil, api.Permanent(errors.New("file append intent size conflict"))
		}
		if err := f.Truncate(intent.Position); err != nil {
			_ = f.Close()
			return nil, err
		}
		if err := syncData(f); err != nil {
			_ = f.Close()
			s.invalidateCapacity()
			return nil, err
		}
		s.adjustCapacity(intent.Position - actualSize)
		stream.Position = intent.Position
		stream.AppendIntent = nil
		actualSize = intent.Position
	}
	if actualSize != stream.Position {
		_ = f.Close()
		return nil, api.Permanent(fmt.Errorf("file position mismatch: actual=%d state=%d", actualSize, stream.Position))
	}
	if _, err := f.Seek(stream.Position, io.SeekStart); err != nil {
		_ = f.Close()
		return nil, err
	}
	crc := crc64.New(crc64.MakeTable(crc64.ECMA))
	if len(stream.CRC64State) > 0 {
		if err := crc.(encoding.BinaryUnmarshaler).UnmarshalBinary(stream.CRC64State); err != nil {
			_ = f.Close()
			return nil, api.Permanent(fmt.Errorf("decode durable file CRC64 checkpoint: %w", err))
		}
	} else if stream.Position > 0 {
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			_ = f.Close()
			return nil, err
		}
		if _, err := io.Copy(crc, io.LimitReader(f, stream.Position)); err != nil {
			_ = f.Close()
			return nil, err
		}
		if _, err := f.Seek(stream.Position, io.SeekStart); err != nil {
			_ = f.Close()
			return nil, err
		}
	}
	w := &writer{stream: stream, kind: streamRef.Kind, resource: resource, metadata: metadata.Clone(), family: family, file: f, crc: crc}
	s.writers[streamRef.ID] = w
	if err := syncDir(dir); err != nil {
		_ = f.Close()
		delete(s.writers, streamRef.ID)
		return nil, err
	}
	if err := s.state.PutSinkStream(name, stream); err != nil {
		_ = f.Close()
		delete(s.writers, streamRef.ID)
		return nil, err
	}
	return w, nil
}

func (s *fileSink) rollover(w *writer) error {
	if err := s.closeGeneration(w); err != nil {
		return err
	}
	return s.startNextGeneration(w)
}

func (s *fileSink) startNextGeneration(w *writer) error {
	if s.cfg.MaxFiles <= 0 || w.stream.Generation >= uint64(s.cfg.MaxFiles-1) {
		return api.Permanent(errors.New("durable file generation limit reached"))
	}
	nextGeneration := w.stream.Generation + 1
	dir, err := familyPath(s.cfg.Root, w.family)
	if err != nil {
		return err
	}
	path, err := safeObjectPath(s.cfg.Root, w.family.DataKey(nextGeneration))
	if err != nil {
		return err
	}
	objectKey := w.family.DataKey(nextGeneration)
	if w.stream.GenerationTransition == nil {
		w.stream.GenerationTransition = &state.GenerationTransition{FromGeneration: w.stream.Generation, ToGeneration: nextGeneration, ObjectKey: objectKey}
		if err := s.state.PutSinkStream(name, w.stream); err != nil {
			w.stream.GenerationTransition = nil
			return err
		}
	} else if w.stream.GenerationTransition.FromGeneration != w.stream.Generation || w.stream.GenerationTransition.ToGeneration != nextGeneration || w.stream.GenerationTransition.ObjectKey != objectKey {
		return api.Permanent(errors.New("generation transition conflicts with checkpoint"))
	}
	f, err := openNoFollow(path)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	if info.Size() != 0 {
		_ = f.Close()
		return api.Permanent(errors.New("refusing to adopt next generation"))
	}
	device, inode, err := fileIdentity(info)
	if err != nil {
		_ = f.Close()
		return err
	}
	if err := syncDir(dir); err != nil {
		_ = f.Close()
		return err
	}
	w.stream.Generation = nextGeneration
	w.stream.Position = 0
	w.stream.Device = device
	w.stream.Inode = inode
	w.stream.CRC64State = nil
	w.stream.CurrentClosed = false
	w.stream.ObjectKey = objectKey
	w.stream.GenerationTransition = nil
	w.file = f
	w.crc = crc64.New(crc64.MakeTable(crc64.ECMA))
	return s.state.PutSinkStream(name, w.stream)
}

func (s *fileSink) closeGeneration(w *writer) error {
	if w.file == nil {
		return nil
	}
	if w.stream.AppendIntent != nil {
		return api.Permanent(errors.New("cannot close durable-file generation with an unresolved append intent"))
	}
	if err := syncData(w.file); err != nil {
		return err
	}
	if err := w.file.Close(); err != nil {
		return err
	}
	w.file = nil
	w.stream.CurrentClosed = true
	crc := strconv.FormatUint(w.crc.Sum64(), 10)
	object := state.ClosedObject{Key: filepath.ToSlash(w.stream.ObjectKey), Generation: w.stream.Generation, Size: w.stream.Position, CRC64: crc}
	if len(w.stream.ClosedObjects) == 0 || w.stream.ClosedObjects[len(w.stream.ClosedObjects)-1].Generation != object.Generation {
		w.stream.ClosedObjects = append(w.stream.ClosedObjects, object)
	}
	return s.state.PutSinkStream(name, w.stream)
}

func familyPath(root string, family objectlayout.Family) (string, error) {
	if family.Directory() == "" {
		return "", api.Permanent(errors.New("file object family has an empty directory"))
	}
	return safeObjectPath(root, family.Directory())
}

func writeFull(file *os.File, data []byte) error {
	for len(data) > 0 {
		written, err := file.Write(data)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		data = data[written:]
	}
	return nil
}

func fileAppendIntent(stream state.SinkStream, length int64, digest [sha256.Size]byte) state.AppendIntent {
	return state.AppendIntent{
		Position: stream.Position,
		Length:   length,
		SHA256:   hex.EncodeToString(digest[:]),
		Device:   stream.Device,
		Inode:    stream.Inode,
	}
}

func recoverAppend(w *writer) error {
	intent := w.stream.AppendIntent
	if intent == nil {
		return nil
	}
	if w.file == nil {
		return errors.New("cannot recover append without an open file")
	}
	if err := w.file.Truncate(intent.Position); err != nil {
		return err
	}
	if err := syncData(w.file); err != nil {
		return err
	}
	if _, err := w.file.Seek(intent.Position, io.SeekStart); err != nil {
		return err
	}
	crc := crc64.New(crc64.MakeTable(crc64.ECMA))
	if len(w.stream.CRC64State) > 0 {
		if err := crc.(encoding.BinaryUnmarshaler).UnmarshalBinary(w.stream.CRC64State); err != nil {
			return err
		}
	}
	w.crc = crc
	return nil
}

func (s *fileSink) recoverFailedAppend(w *writer, cause error) error {
	recoveryErr := recoverAppend(w)
	if recoveryErr != nil {
		s.invalidateCapacity()
	}
	return errors.Join(cause, recoveryErr)
}

func (s *fileSink) reserveCapacity(additional int64) error {
	if additional < 0 {
		return api.Permanent(errors.New("durable file capacity reservation cannot be negative"))
	}
	if s.cfg.Root == "" || s.cfg.MaxTotalBytes <= 0 {
		return nil
	}
	if !s.capacityKnown {
		used, err := measureCapacity(s.cfg.Root)
		if err != nil {
			return err
		}
		s.capacityUsed = used
		s.capacityKnown = true
	}
	if s.capacityUsed > s.cfg.MaxTotalBytes || additional > s.cfg.MaxTotalBytes-s.capacityUsed {
		if additional > s.cfg.MaxTotalBytes {
			return api.Permanent(fmt.Errorf("durable file reservation %d exceeds total-byte limit %d", additional, s.cfg.MaxTotalBytes))
		}
		return capacityExhaustedError{limit: s.cfg.MaxTotalBytes}
	}
	return nil
}

func measureCapacity(root string) (int64, error) {
	var used int64
	err := filepath.WalkDir(root, func(_ string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Size() > math.MaxInt64-used {
			return api.Permanent(errors.New("durable file capacity exceeds int64"))
		}
		used += info.Size()
		return nil
	})
	return used, err
}

func (s *fileSink) adjustCapacity(delta int64) {
	if !s.capacityKnown {
		return
	}
	if (delta > 0 && s.capacityUsed > math.MaxInt64-delta) || delta == math.MinInt64 || (delta < 0 && s.capacityUsed < -delta) {
		s.invalidateCapacity()
		return
	}
	s.capacityUsed += delta
}

func (s *fileSink) invalidateCapacity() {
	s.capacityUsed = 0
	s.capacityKnown = false
}

func (s *fileSink) validateClosedFileLayout(resource api.Resource, metadata api.StreamMetadata, stream state.SinkStream, format streamformat.Format) error {
	family, err := streamformat.ResolveFamily(format, "", api.StreamRef{ID: stream.StreamRef, Kind: format.Kind()}, resource, metadata)
	if err != nil {
		return api.Permanent(fmt.Errorf("resolve file object family: %w", err))
	}
	if _, err := familyPath(s.cfg.Root, family); err != nil {
		return err
	}
	expectedClosed := stream.Generation
	if stream.CurrentClosed {
		if expectedClosed == ^uint64(0) {
			return api.Permanent(errors.New("durable file checkpoint generation overflows closed-object count"))
		}
		expectedClosed++
	}
	if uint64(len(stream.ClosedObjects)) != expectedClosed {
		return api.Permanent(fmt.Errorf("durable file checkpoint has %d closed objects for generation %d (closed=%t)", len(stream.ClosedObjects), stream.Generation, stream.CurrentClosed))
	}
	for index, object := range stream.ClosedObjects {
		if object.Generation != uint64(index) {
			return api.Permanent(fmt.Errorf("closed file generation %d is not continuous at index %d", object.Generation, index))
		}
		expectedKey := family.DataKey(object.Generation)
		if object.Key != expectedKey {
			return api.Permanent(fmt.Errorf("closed file generation %d has unexpected object key %q", object.Generation, object.Key))
		}
	}
	if stream.CurrentClosed {
		current := stream.ClosedObjects[len(stream.ClosedObjects)-1]
		if current.Key != stream.ObjectKey || current.Size != stream.Position {
			return api.Permanent(errors.New("closed file checkpoint does not match the current generation"))
		}
	}
	return nil
}

func (s *fileSink) verifyClosedFiles(ctx context.Context, resource api.Resource, metadata api.StreamMetadata, stream state.SinkStream, format streamformat.Format) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.validateClosedFileLayout(resource, metadata, stream, format); err != nil {
		return err
	}
	for _, object := range stream.ClosedObjects {
		if err := ctx.Err(); err != nil {
			return err
		}
		path, err := safeObjectPath(s.cfg.Root, object.Key)
		if err != nil {
			return err
		}
		file, err := openNoFollowRead(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return api.Permanent(fmt.Errorf("closed file %s is missing: %w", object.Key, err))
			}
			return err
		}
		info, err := file.Stat()
		if err != nil {
			_ = file.Close()
			return err
		}
		if !info.Mode().IsRegular() || info.Size() != object.Size {
			_ = file.Close()
			return api.Permanent(fmt.Errorf("closed file %s changed size or type", object.Key))
		}
		checksum := crc64.New(crc64.MakeTable(crc64.ECMA))
		if _, err := io.Copy(checksum, contextReader{ctx: ctx, reader: file}); err != nil {
			_ = file.Close()
			return err
		}
		if err := file.Close(); err != nil {
			return err
		}
		if strconv.FormatUint(checksum.Sum64(), 10) != object.CRC64 {
			return api.Permanent(fmt.Errorf("closed file %s changed checksum", object.Key))
		}
	}
	return nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(buffer)
}

func readNoFollow(path string) ([]byte, error) {
	file, err := openNoFollowRead(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return io.ReadAll(file)
}

func safeObjectPath(root, key string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(key))
	if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", api.Permanent(errors.New("unsafe durable file object key"))
	}
	path := filepath.Join(root, clean)
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", api.Permanent(errors.New("durable file object escapes root"))
	}
	return path, nil
}

func quarantineOrphan(root, path string) error {
	dir := filepath.Join(root, ".quarantine")
	if err := mkdirAllNoFollow(dir, 0o700); err != nil {
		return err
	}
	digest := sha256.Sum256([]byte(path))
	destination := filepath.Join(dir, fmt.Sprintf("%d-%s-%s", time.Now().UTC().UnixNano(), hex.EncodeToString(digest[:6]), filepath.Base(path)))
	if err := os.Rename(path, destination); err != nil {
		return err
	}
	if err := syncDir(filepath.Dir(path)); err != nil {
		return err
	}
	return syncDir(dir)
}
