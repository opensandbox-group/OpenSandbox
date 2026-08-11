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

package oss

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc64"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/nodeagent/pkg/api"
	"github.com/alibaba/opensandbox/nodeagent/pkg/marker"
	lineformat "github.com/alibaba/opensandbox/nodeagent/pkg/sink"
	"github.com/alibaba/opensandbox/nodeagent/pkg/state"
	aliyunoss "github.com/aliyun/aliyun-oss-go-sdk/oss"
)

type memoryObject struct {
	data               []byte
	metadata           map[string]string
	objectType         string
	sealedTime         string
	nextAppendPosition *int64
}

type fakeBackend struct {
	mu           sync.Mutex
	objects      map[string]memoryObject
	appendResult string
	appendError  error
	headError    error
	preflights   int
	contexts     map[string]context.Context
}

type blockingAppendBackend struct {
	*fakeBackend
	blockKey string
	entered  chan struct{}
	release  chan struct{}
	once     sync.Once
}

type cancelAfterAppendBackend struct {
	*fakeBackend
	cancel context.CancelFunc
	once   sync.Once
}

type failFinalCheckpointStore struct {
	*state.DB
	fail bool
}

func (s *failFinalCheckpointStore) PutSinkStream(sinkName string, stream state.SinkStream) error {
	if s.fail && stream.Position > 0 && stream.AppendIntent == nil {
		s.fail = false
		return errors.New("injected final checkpoint failure")
	}
	return s.DB.PutSinkStream(sinkName, stream)
}

func (b *blockingAppendBackend) Append(ctx context.Context, key string, data []byte, position int64, metadata map[string]string) (int64, error) {
	if key == b.blockKey {
		b.once.Do(func() { close(b.entered) })
		select {
		case <-b.release:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	return b.fakeBackend.Append(ctx, key, data, position, metadata)
}

func (b *cancelAfterAppendBackend) Append(ctx context.Context, key string, data []byte, position int64, metadata map[string]string) (int64, error) {
	next, err := b.fakeBackend.Append(ctx, key, data, position, metadata)
	if err != nil {
		return next, err
	}
	canceled := false
	b.once.Do(func() {
		canceled = true
		b.cancel()
	})
	if canceled {
		return position, ctx.Err()
	}
	return next, nil
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{objects: make(map[string]memoryObject), contexts: make(map[string]context.Context)}
}

func (b *fakeBackend) Preflight(ctx context.Context, _ string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.contexts["preflight"] = ctx
	b.preflights++
	return ctx.Err()
}

func (b *fakeBackend) Append(ctx context.Context, key string, data []byte, position int64, metadata map[string]string) (int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.contexts["append"] = ctx
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	object, found := b.objects[key]
	if found && object.objectType != appendableObjectType {
		return position, aliyunoss.ServiceError{StatusCode: http.StatusConflict, Code: "ObjectNotAppendable"}
	}
	if found && object.sealedTime != "" {
		return position, aliyunoss.ServiceError{StatusCode: http.StatusConflict, Code: "AppendSealedObjectNotAllowed"}
	}
	if int64(len(object.data)) != position {
		return position, aliyunoss.ServiceError{StatusCode: http.StatusConflict, Code: "PositionNotEqualToLength"}
	}
	if b.appendError != nil {
		return position, b.appendError
	}
	result := b.appendResult
	b.appendResult = ""
	if result == "before" {
		return 0, errors.New("connection reset before append")
	}
	if result == "unexpected-before" {
		return position + 1, nil
	}
	if position == 0 {
		object.metadata = cloneMap(metadata)
		object.objectType = appendableObjectType
	}
	if result == "foreign-same-size" {
		object.metadata["nodeagent-stream-ref"] = "foreign-stream"
		object.data = append(object.data, data...)
		object.nextAppendPosition = nil
		b.objects[key] = object
		return 0, errors.New("connection reset after foreign append")
	}
	object.data = append(object.data, data...)
	object.nextAppendPosition = nil
	b.objects[key] = object
	if result == "after" {
		return 0, errors.New("connection reset after append")
	}
	if result == "unexpected-after" {
		return int64(len(object.data) + 1), nil
	}
	if result == "unexpected-conflict" {
		object.data = append(object.data, 'x')
		b.objects[key] = object
		return int64(len(object.data) + 1), nil
	}
	return int64(len(object.data)), nil
}

func (b *fakeBackend) Head(ctx context.Context, key string) (objectMetadata, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.contexts["head"] = ctx
	if err := ctx.Err(); err != nil {
		return objectMetadata{}, err
	}
	if b.headError != nil {
		return objectMetadata{}, b.headError
	}
	object, found := b.objects[key]
	if !found {
		return objectMetadata{}, errObjectNotFound
	}
	checksum := crc64.Checksum(object.data, crc64.MakeTable(crc64.ECMA))
	var nextAppendPosition *int64
	if object.nextAppendPosition != nil {
		next := *object.nextAppendPosition
		nextAppendPosition = &next
	} else if object.objectType == appendableObjectType {
		next := int64(len(object.data))
		nextAppendPosition = &next
	}
	return objectMetadata{
		Size:               int64(len(object.data)),
		CRC64:              strconv.FormatUint(checksum, 10),
		Metadata:           cloneMap(object.metadata),
		ObjectType:         object.objectType,
		NextAppendPosition: nextAppendPosition,
		SealedTime:         object.sealedTime,
	}, nil
}

func (b *fakeBackend) PutMarker(ctx context.Context, key string, data []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.contexts["put-marker"] = ctx
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, exists := b.objects[key]; exists {
		return errors.New("forbid overwrite")
	}
	b.objects[key] = memoryObject{data: append([]byte(nil), data...), objectType: "Normal"}
	return nil
}

func (b *fakeBackend) Get(ctx context.Context, key string) ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.contexts["get"] = ctx
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	object, found := b.objects[key]
	if !found {
		return nil, errObjectNotFound
	}
	return append([]byte(nil), object.data...), nil
}

func TestParseObjectMetadataCapturesAppendProtocolHeaders(t *testing.T) {
	header := make(http.Header)
	header.Set("Content-Length", "5")
	header.Set(aliyunoss.HTTPHeaderOssCRC64, "123")
	header.Set(aliyunoss.HTTPHeaderOssNextAppendPosition, "5")
	header.Set(ossObjectTypeHeader, appendableObjectType)
	header.Set(ossSealedTimeHeader, "Wed, 07 May 2025 23:00:00 GMT")
	header.Set(aliyunoss.HTTPHeaderOssMetaPrefix+"nodeagent-writer-id", "writer")

	metadata, err := parseObjectMetadata(header)
	if err != nil {
		t.Fatal(err)
	}
	if metadata.Size != 5 || metadata.CRC64 != "123" || metadata.ObjectType != appendableObjectType || metadata.SealedTime == "" {
		t.Fatalf("metadata=%+v", metadata)
	}
	if metadata.NextAppendPosition == nil || *metadata.NextAppendPosition != 5 {
		t.Fatalf("next append position=%v", metadata.NextAppendPosition)
	}
	if metadata.Metadata["nodeagent-writer-id"] != "writer" {
		t.Fatalf("user metadata=%+v", metadata.Metadata)
	}
}

func TestParseObjectMetadataRejectsInvalidProtocolPositions(t *testing.T) {
	for _, test := range []struct {
		name          string
		contentLength string
		nextPosition  string
	}{
		{name: "invalid content length", contentLength: "invalid", nextPosition: "0"},
		{name: "negative content length", contentLength: "-1", nextPosition: "0"},
		{name: "invalid next position", contentLength: "0", nextPosition: "invalid"},
		{name: "negative next position", contentLength: "0", nextPosition: "-1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			header := make(http.Header)
			header.Set("Content-Length", test.contentLength)
			header.Set(aliyunoss.HTTPHeaderOssNextAppendPosition, test.nextPosition)
			_, err := parseObjectMetadata(header)
			if err == nil || api.IsRetryableError(err) {
				t.Fatalf("error=%v retryable=%v", err, api.IsRetryableError(err))
			}
		})
	}
}

func TestOSSAppendUnknownResultAndFinalize(t *testing.T) {
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	backend := newFakeBackend()
	backend.appendResult = "after"
	sink := newWithBackend(testOSSConfig(db), db, backend)
	if err := sink.Preflight(context.Background()); err != nil {
		t.Fatal(err)
	}
	batch, resource := testOSSBatch()
	if err := sink.Consume(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
	key := objectKey("logs", resource, 0)
	object := backend.objects[key]
	want := lineformat.EncodeBatch(batch)
	if !bytes.Equal(object.data, want) || object.metadata["nodeagent-target-id"] != "target" || object.metadata["nodeagent-stream-ref"] != batch.StreamRef.ID {
		t.Fatalf("object=%+v data=%q", object.metadata, object.data)
	}
	request := api.FinalizeRequest{FinalizeID: "final", TargetID: "target", StreamRef: batch.StreamRef, Revision: 1, CoverageStartedAt: time.Date(2026, 7, 23, 9, 58, 0, 0, time.UTC), Resource: resource, FinalizedAt: time.Date(2026, 7, 23, 10, 5, 0, 0, time.UTC)}
	if err := sink.Finalize(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	markerRaw := backend.objects[markerKey("logs", resource, 1)].data
	value, err := marker.Decode(markerRaw)
	if err != nil || len(value.Objects) != 1 || value.Objects[0].Size != int64(len(want)) {
		t.Fatalf("marker=%+v err=%v", value, err)
	}
	if backend.preflights != 2 {
		t.Fatalf("preflight count=%d", backend.preflights)
	}
	if _, _, cached := sink.cachedStream(batch.StreamRef.ID); cached {
		t.Fatal("finalized stream remained in the OSS memory cache")
	}
}

func TestOSSRetryWhenUnknownResultDidNotAppend(t *testing.T) {
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	backend := newFakeBackend()
	backend.appendResult = "before"
	sink := newWithBackend(testOSSConfig(db), db, backend)
	batch, resource := testOSSBatch()
	if err := sink.Consume(context.Background(), batch); err == nil {
		t.Fatal("unknown result unexpectedly succeeded")
	}
	if err := sink.Consume(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
	if got := backend.objects[objectKey("logs", resource, 0)].data; !bytes.Equal(got, lineformat.EncodeBatch(batch)) {
		t.Fatalf("data=%q", got)
	}
}

func TestOSSRetainsSameProcessIntentWhenRecoveryContextExpires(t *testing.T) {
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	backend := &cancelAfterAppendBackend{fakeBackend: newFakeBackend(), cancel: cancel}
	sink := newWithBackend(testOSSConfig(db), db, backend)
	batch, resource := testOSSBatch()

	err = sink.Consume(ctx, batch)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("first consume error=%v", err)
	}
	stream, _, cached := sink.cachedStream(batch.StreamRef.ID)
	if !cached || stream.AppendIntent == nil {
		t.Fatalf("stream=%+v cached=%v", stream, cached)
	}
	if err := sink.Consume(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
	if got, want := backend.objects[objectKey("logs", resource, 0)].data, lineformat.EncodeBatch(batch); !bytes.Equal(got, want) {
		t.Fatalf("same-process recovery duplicated data: got=%q want=%q", got, want)
	}
}

func TestOSSRecoversUnexpectedNextPosition(t *testing.T) {
	for _, test := range []struct {
		name          string
		appendResult  string
		firstSucceeds bool
	}{{name: "remote-accepted", appendResult: "unexpected-after", firstSucceeds: true}, {name: "remote-unchanged", appendResult: "unexpected-before"}} {
		t.Run(test.name, func(t *testing.T) {
			db, err := state.Open(t.TempDir(), "target", 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			backend := newFakeBackend()
			backend.appendResult = test.appendResult
			sink := newWithBackend(testOSSConfig(db), db, backend)
			batch, resource := testOSSBatch()
			err = sink.Consume(context.Background(), batch)
			if test.firstSucceeds {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				if err == nil || !api.IsRetryableError(err) {
					t.Fatalf("first consume error=%v retryable=%v", err, api.IsRetryableError(err))
				}
				if _, cached := sink.streams[batch.StreamRef.ID]; cached {
					t.Fatal("stream with an unresolved append result remained cached")
				}
				if err := sink.Consume(context.Background(), batch); err != nil {
					t.Fatal(err)
				}
			}
			if got, want := backend.objects[objectKey("logs", resource, 0)].data, lineformat.EncodeBatch(batch); !bytes.Equal(got, want) {
				t.Fatalf("data=%q want=%q", got, want)
			}
		})
	}
}

func TestOSSRejectsUnexpectedRemotePositionAsNonRetryable(t *testing.T) {
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	backend := newFakeBackend()
	backend.appendResult = "unexpected-conflict"
	sink := newWithBackend(testOSSConfig(db), db, backend)
	batch, _ := testOSSBatch()
	err = sink.Consume(context.Background(), batch)
	if err == nil || api.IsRetryableError(err) || !strings.Contains(err.Error(), "position conflict") {
		t.Fatalf("consume error=%v retryable=%v", err, api.IsRetryableError(err))
	}
	if _, cached := sink.streams[batch.StreamRef.ID]; cached {
		t.Fatal("conflicting stream remained cached")
	}
	stream, found, stateErr := db.GetSinkStream(name, batch.StreamRef.ID)
	if stateErr != nil || !found || stream.AppendIntent == nil {
		t.Fatalf("stream=%+v found=%v err=%v", stream, found, stateErr)
	}
}

func TestOSSRestartReplaysWhenUnknownAppendWasAccepted(t *testing.T) {
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	backend := newFakeBackend()
	cfg := testOSSConfig(db)
	batch, resource := testOSSBatch()
	data := lineformat.EncodeBatch(batch)
	key := objectKey("logs", resource, 0)
	metadata := testOSSMetadata(cfg, batch.StreamRef, resource, 0)
	backend.objects[key] = memoryObject{data: append([]byte(nil), data...), metadata: metadata, objectType: appendableObjectType}
	digest := sha256.Sum256(data)
	if err := db.PutSinkStream(name, state.SinkStream{StreamRef: batch.StreamRef.ID, ObjectKey: key, AppendIntent: &state.AppendIntent{Position: 0, Length: int64(len(data)), SHA256: hex.EncodeToString(digest[:])}}); err != nil {
		t.Fatal(err)
	}
	sink := newWithBackend(cfg, db, backend)
	if err := sink.Consume(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
	want := append(append([]byte(nil), data...), data...)
	if got := backend.objects[key].data; !bytes.Equal(got, want) {
		t.Fatalf("restart must replay accepted but unacknowledged append: got=%q want=%q", got, want)
	}
}

func TestOSSRestartRejectsConflictingPosition(t *testing.T) {
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	backend := newFakeBackend()
	cfg := testOSSConfig(db)
	batch, resource := testOSSBatch()
	data := lineformat.EncodeBatch(batch)
	key := objectKey("logs", resource, 0)
	backend.objects[key] = memoryObject{data: append(append([]byte(nil), data...), 'x'), metadata: testOSSMetadata(cfg, batch.StreamRef, resource, 0), objectType: appendableObjectType}
	digest := sha256.Sum256(data)
	if err := db.PutSinkStream(name, state.SinkStream{StreamRef: batch.StreamRef.ID, ObjectKey: key, AppendIntent: &state.AppendIntent{Position: 0, Length: int64(len(data)), SHA256: hex.EncodeToString(digest[:])}}); err != nil {
		t.Fatal(err)
	}
	sink := newWithBackend(cfg, db, backend)
	if err := sink.Consume(context.Background(), batch); err == nil || api.IsRetryableError(err) {
		t.Fatalf("conflicting remote position error=%v retryable=%v", err, api.IsRetryableError(err))
	}
}

func TestOSSRestartRejectsForeignZeroLengthObject(t *testing.T) {
	for _, withIntent := range []bool{false, true} {
		t.Run(fmt.Sprintf("intent-%t", withIntent), func(t *testing.T) {
			db, err := state.Open(t.TempDir(), "target", 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			backend := newFakeBackend()
			cfg := testOSSConfig(db)
			batch, resource := testOSSBatch()
			key := objectKey("logs", resource, 0)
			stream := state.SinkStream{StreamRef: batch.StreamRef.ID, ObjectKey: key}
			if withIntent {
				data := lineformat.EncodeBatch(batch)
				digest := sha256.Sum256(data)
				stream.AppendIntent = &state.AppendIntent{Position: 0, Length: int64(len(data)), SHA256: hex.EncodeToString(digest[:])}
			}
			if err := db.PutSinkStream(name, stream); err != nil {
				t.Fatal(err)
			}
			backend.objects[key] = memoryObject{metadata: map[string]string{"nodeagent-writer-id": "foreign"}, objectType: appendableObjectType}
			sink := newWithBackend(cfg, db, backend)
			err = sink.Consume(context.Background(), batch)
			if err == nil || api.IsRetryableError(err) || !strings.Contains(err.Error(), "metadata") {
				t.Fatalf("foreign zero-length object error=%v retryable=%v", err, api.IsRetryableError(err))
			}
			if len(backend.objects[key].data) != 0 {
				t.Fatalf("foreign object was modified: %q", backend.objects[key].data)
			}
		})
	}
}

func TestOSSRestartAcceptsOwnedZeroLengthObject(t *testing.T) {
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	backend := newFakeBackend()
	cfg := testOSSConfig(db)
	batch, resource := testOSSBatch()
	key := objectKey("logs", resource, 0)
	if err := db.PutSinkStream(name, state.SinkStream{StreamRef: batch.StreamRef.ID, ObjectKey: key}); err != nil {
		t.Fatal(err)
	}
	backend.objects[key] = memoryObject{metadata: testOSSMetadata(cfg, batch.StreamRef, resource, 0), objectType: appendableObjectType}
	sink := newWithBackend(cfg, db, backend)
	if err := sink.Consume(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
	if got, want := backend.objects[key].data, lineformat.EncodeBatch(batch); !bytes.Equal(got, want) {
		t.Fatalf("data=%q want=%q", got, want)
	}
}

func TestOSSRestartTreatsMissingCommittedObjectAsNonRetryable(t *testing.T) {
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	batch, resource := testOSSBatch()
	key := objectKey("logs", resource, 0)
	if err := db.PutSinkStream(name, state.SinkStream{StreamRef: batch.StreamRef.ID, ObjectKey: key, Position: 1}); err != nil {
		t.Fatal(err)
	}
	sink := newWithBackend(testOSSConfig(db), db, newFakeBackend())
	err = sink.Consume(context.Background(), batch)
	if err == nil || api.IsRetryableError(err) || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("missing object error=%v retryable=%v", err, api.IsRetryableError(err))
	}
}

func TestOSSRejectsClosedObjectCountMismatchBeforeAppend(t *testing.T) {
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	backend := newFakeBackend()
	batch, resource := testOSSBatch()
	key := objectKey("logs", resource, 0)
	stream := state.SinkStream{StreamRef: batch.StreamRef.ID, ObjectKey: key, CurrentClosed: true}
	if err := db.PutSinkStream(name, stream); err != nil {
		t.Fatal(err)
	}
	sink := newWithBackend(testOSSConfig(db), db, backend)
	err = sink.Consume(context.Background(), batch)
	if err == nil || api.IsRetryableError(err) || !strings.Contains(err.Error(), "closed objects") {
		t.Fatalf("layout error=%v retryable=%v", err, api.IsRetryableError(err))
	}
	if len(backend.objects) != 0 {
		t.Fatalf("invalid checkpoint created objects: %+v", backend.objects)
	}
}

func TestOSSRolloverKeepsMarkerGenerationsContinuous(t *testing.T) {
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	backend := newFakeBackend()
	batch, resource := testOSSBatch()
	cfg := testOSSConfig(db)
	cfg.MaxObjectBytes = int64(len(lineformat.EncodeBatch(batch)))
	sink := newWithBackend(cfg, db, backend)
	for index := 0; index < 3; index++ {
		current := batch
		current.Items = append([]api.BatchItem(nil), batch.Items...)
		current.Items[0].RecordID = "record-" + strconv.Itoa(index)
		if err := sink.Consume(context.Background(), current); err != nil {
			t.Fatal(err)
		}
	}
	request := api.FinalizeRequest{FinalizeID: "final", TargetID: "target", StreamRef: batch.StreamRef, Revision: 1, CoverageStartedAt: time.Date(2026, 7, 23, 9, 58, 0, 0, time.UTC), Resource: resource, FinalizedAt: time.Date(2026, 7, 23, 10, 5, 0, 0, time.UTC)}
	if err := sink.Finalize(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	value, err := marker.Decode(backend.objects[markerKey("logs", resource, 1)].data)
	if err != nil {
		t.Fatal(err)
	}
	if len(value.Objects) != 3 {
		t.Fatalf("objects=%+v", value.Objects)
	}
	for generation, object := range value.Objects {
		if object.Generation != uint64(generation) {
			t.Fatalf("object %d generation=%d", generation, object.Generation)
		}
	}
}

type backendContextKey struct{}

func TestOSSPropagatesOperationContextToBackend(t *testing.T) {
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	backend := newFakeBackend()
	sink := newWithBackend(testOSSConfig(db), db, backend)
	ctx := context.WithValue(context.Background(), backendContextKey{}, "request")
	if err := sink.Preflight(ctx); err != nil {
		t.Fatal(err)
	}
	batch, resource := testOSSBatch()
	if err := sink.Consume(ctx, batch); err != nil {
		t.Fatal(err)
	}
	request := api.FinalizeRequest{FinalizeID: "final", TargetID: "target", StreamRef: batch.StreamRef, Revision: 1, CoverageStartedAt: time.Date(2026, 7, 23, 9, 58, 0, 0, time.UTC), Resource: resource, FinalizedAt: time.Date(2026, 7, 23, 10, 5, 0, 0, time.UTC)}
	if err := sink.Finalize(ctx, request); err != nil {
		t.Fatal(err)
	}
	if err := sink.Finalize(ctx, request); err != nil {
		t.Fatal(err)
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	for _, operation := range []string{"preflight", "head", "append", "put-marker", "get"} {
		operationCtx := backend.contexts[operation]
		if operationCtx == nil || operationCtx.Value(backendContextKey{}) != "request" {
			t.Fatalf("%s context=%v", operation, operationCtx)
		}
	}
}

func TestOSSRejectsBatchLargerThanObjectLimit(t *testing.T) {
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	backend := newFakeBackend()
	cfg := testOSSConfig(db)
	cfg.MaxObjectBytes = 4
	sink := newWithBackend(cfg, db, backend)
	batch, _ := testOSSBatch()
	err = sink.Consume(context.Background(), batch)
	if err == nil || !strings.Contains(err.Error(), "exceeds per-generation limit") {
		t.Fatalf("consume error=%v", err)
	}
	if api.IsRetryableError(err) {
		t.Fatalf("oversized batch error is retryable: %v", err)
	}
	if len(backend.objects) != 0 {
		t.Fatalf("oversized batch created objects: %+v", backend.objects)
	}
}

func TestOSSObjectLimitCheckDoesNotOverflow(t *testing.T) {
	if !appendExceedsObjectLimit(math.MaxInt64-1, 2, math.MaxInt64) {
		t.Fatal("overflowing append did not trigger rollover")
	}
	if appendExceedsObjectLimit(math.MaxInt64-1, 1, math.MaxInt64) {
		t.Fatal("exactly fitting append triggered rollover")
	}
	if !appendExceedsObjectLimit(11, 1, 10) {
		t.Fatal("position beyond the configured limit did not trigger rollover")
	}
	if appendExceedsObjectLimit(0, math.MaxInt64, math.MaxInt64) {
		t.Fatal("first append within the limit triggered rollover")
	}
}

func TestOSSRejectsOversizedMetadataAsNonRetryable(t *testing.T) {
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	backend := newFakeBackend()
	sink := newWithBackend(testOSSConfig(db), db, backend)
	batch, _ := testOSSBatch()
	batch.Items[0].Record.Resource.PodName = strings.Repeat("x", 8<<10)
	err = sink.Consume(context.Background(), batch)
	if err == nil || !strings.Contains(err.Error(), "metadata exceeds 8 KiB") {
		t.Fatalf("consume error=%v", err)
	}
	if api.IsRetryableError(err) {
		t.Fatalf("oversized metadata error is retryable: %v", err)
	}
	if len(backend.objects) != 0 {
		t.Fatalf("oversized metadata created objects: %+v", backend.objects)
	}
}

func TestOSSMetadataLimitCountsFullHTTPHeaderNames(t *testing.T) {
	metadata := map[string]string{"key": strings.Repeat("x", (8<<10)-len(aliyunoss.HTTPHeaderOssMetaPrefix)-len("key"))}
	if got := metadataBytes(metadata); got != 8<<10 {
		t.Fatalf("metadata bytes=%d want=%d", got, 8<<10)
	}
	metadata["key"] += "x"
	if got := metadataBytes(metadata); got != 8<<10+1 {
		t.Fatalf("metadata bytes=%d want=%d", got, 8<<10+1)
	}
}

func TestOSSAllowsDifferentStreamsToAppendConcurrently(t *testing.T) {
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	first, firstResource := testOSSBatch()
	backend := &blockingAppendBackend{
		fakeBackend: newFakeBackend(),
		blockKey:    objectKey("logs", firstResource, 0),
		entered:     make(chan struct{}),
		release:     make(chan struct{}),
	}
	sink := newWithBackend(testOSSConfig(db), db, backend)
	second, secondResource := testOSSBatch()
	second.Items = append([]api.BatchItem(nil), second.Items...)
	secondResource.SandboxID = "sb-2"
	secondResource.PodUID = "uid-2"
	second.Items[0].Record.Resource = secondResource
	for suffix := 2; ; suffix++ {
		second.StreamRef.ID = fmt.Sprintf("container-logs/uid-%d/sandbox", suffix)
		if sink.streamLock(first.StreamRef.ID) != sink.streamLock(second.StreamRef.ID) {
			break
		}
	}

	firstDone := make(chan error, 1)
	go func() { firstDone <- sink.Consume(context.Background(), first) }()
	select {
	case <-backend.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("first append did not reach backend")
	}
	secondDone := make(chan error, 1)
	go func() { secondDone <- sink.Consume(context.Background(), second) }()
	select {
	case err := <-secondDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second stream was blocked by first stream's append")
	}
	close(backend.release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
}

func TestOSSDeterministicServiceErrorsAreNonRetryable(t *testing.T) {
	for _, serviceErr := range []aliyunoss.ServiceError{
		{StatusCode: http.StatusUnauthorized, Code: "InvalidAccessKeyId"},
		{StatusCode: http.StatusForbidden, Code: "AccessDenied"},
		{StatusCode: http.StatusBadRequest, Code: "SecurityTokenExpired"},
		{StatusCode: http.StatusBadRequest, Code: "InvalidSecurityToken"},
		{StatusCode: http.StatusForbidden, Code: "SignatureDoesNotMatch"},
		{StatusCode: http.StatusNotFound, Code: "NoSuchBucket"},
		{StatusCode: http.StatusConflict, Code: "ObjectNotAppendable"},
		{StatusCode: http.StatusConflict, Code: "AppendSealedObjectNotAllowed"},
		{StatusCode: http.StatusBadRequest, Code: "InvalidArgument"},
		{StatusCode: http.StatusConflict, Code: "FileImmutable"},
		{StatusCode: http.StatusForbidden, Code: "KmsServiceNotEnabled"},
		{StatusCode: http.StatusBadRequest, Code: "InvalidObjectName"},
	} {
		err := classifyOSSError(serviceErr)
		if api.IsRetryableError(err) {
			t.Fatalf("service error %s remained retryable", serviceErr.Code)
		}
	}
	for _, serviceErr := range []aliyunoss.ServiceError{
		{StatusCode: http.StatusForbidden, Code: "RequestTimeTooSkewed"},
		{StatusCode: http.StatusConflict, Code: "PositionNotEqualToLength"},
		{StatusCode: http.StatusInternalServerError, Code: "InternalError"},
	} {
		err := classifyOSSError(serviceErr)
		if !api.IsRetryableError(err) {
			t.Fatalf("service error %s became non-retryable: %v", serviceErr.Code, err)
		}
	}
}

func TestOSSNoSuchBucketIsNotTreatedAsAMissingObject(t *testing.T) {
	serviceErr := aliyunoss.ServiceError{StatusCode: http.StatusNotFound, Code: "NoSuchBucket"}
	if isNotFound(serviceErr) {
		t.Fatal("NoSuchBucket was treated as a missing object")
	}
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	backend := newFakeBackend()
	backend.headError = serviceErr
	sink := newWithBackend(testOSSConfig(db), db, backend)
	batch, _ := testOSSBatch()
	err = sink.Consume(context.Background(), batch)
	if err == nil || api.IsRetryableError(err) || !strings.Contains(err.Error(), "NoSuchBucket") {
		t.Fatalf("error=%v retryable=%v", err, api.IsRetryableError(err))
	}
	if _, found, stateErr := db.GetSinkStream(name, batch.StreamRef.ID); stateErr != nil || found {
		t.Fatalf("found=%v state error=%v", found, stateErr)
	}
}

func TestOSSHead404WithoutCodeIsTreatedAsAMissingObject(t *testing.T) {
	serviceErr := aliyunoss.ServiceError{StatusCode: http.StatusNotFound}
	if !isNotFound(serviceErr) {
		t.Fatal("body-less HEAD 404 was not treated as a missing object")
	}
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	backend := newFakeBackend()
	backend.headError = serviceErr
	sink := newWithBackend(testOSSConfig(db), db, backend)
	batch, resource := testOSSBatch()
	if err := sink.Consume(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
	if got := backend.objects[objectKey("logs", resource, 0)].data; !bytes.Equal(got, lineformat.EncodeBatch(batch)) {
		t.Fatalf("data=%q", got)
	}
}

func TestOSSDeterministicAppendFailureStopsRetryingAtCommittedPosition(t *testing.T) {
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	backend := newFakeBackend()
	backend.appendError = aliyunoss.ServiceError{StatusCode: http.StatusConflict, Code: "ObjectNotAppendable"}
	cfg := testOSSConfig(db)
	batch, resource := testOSSBatch()
	key := objectKey(cfg.Prefix, resource, 0)
	if err := db.PutSinkStream(name, state.SinkStream{StreamRef: batch.StreamRef.ID, ObjectKey: key}); err != nil {
		t.Fatal(err)
	}
	backend.objects[key] = memoryObject{metadata: testOSSMetadata(cfg, batch.StreamRef, resource, 0), objectType: appendableObjectType}
	sink := newWithBackend(cfg, db, backend)
	err = sink.Consume(context.Background(), batch)
	if err == nil || api.IsRetryableError(err) || !strings.Contains(err.Error(), "ObjectNotAppendable") {
		t.Fatalf("error=%v retryable=%v", err, api.IsRetryableError(err))
	}
}

func TestOSSRejectsInvalidAppendObjectProtocolHeaders(t *testing.T) {
	for _, test := range []struct {
		name               string
		objectType         string
		sealedTime         string
		nextAppendPosition *int64
		want               string
	}{
		{name: "normal object", objectType: "Normal", want: "not Appendable"},
		{name: "sealed object", objectType: appendableObjectType, sealedTime: "Wed, 07 May 2025 23:00:00 GMT", want: "sealed"},
		{name: "wrong next position", objectType: appendableObjectType, nextAppendPosition: int64Pointer(1), want: "next append position"},
	} {
		t.Run(test.name, func(t *testing.T) {
			db, err := state.Open(t.TempDir(), "target", 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			backend := newFakeBackend()
			cfg := testOSSConfig(db)
			batch, resource := testOSSBatch()
			key := objectKey(cfg.Prefix, resource, 0)
			if err := db.PutSinkStream(name, state.SinkStream{StreamRef: batch.StreamRef.ID, ObjectKey: key}); err != nil {
				t.Fatal(err)
			}
			backend.objects[key] = memoryObject{
				metadata:           testOSSMetadata(cfg, batch.StreamRef, resource, 0),
				objectType:         test.objectType,
				sealedTime:         test.sealedTime,
				nextAppendPosition: test.nextAppendPosition,
			}
			sink := newWithBackend(cfg, db, backend)
			err = sink.Consume(context.Background(), batch)
			if err == nil || api.IsRetryableError(err) || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v retryable=%v", err, api.IsRetryableError(err))
			}
		})
	}
}

func TestOSSRejectsUnsafeObjectKeyResource(t *testing.T) {
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	backend := newFakeBackend()
	sink := newWithBackend(testOSSConfig(db), db, backend)
	batch, _ := testOSSBatch()
	batch.Items[0].Record.Resource.Container = "../../escape"
	err = sink.Consume(context.Background(), batch)
	if err == nil || api.IsRetryableError(err) {
		t.Fatalf("error=%v retryable=%v", err, api.IsRetryableError(err))
	}
	if len(backend.objects) != 0 {
		t.Fatalf("unsafe resource created objects: %+v", backend.objects)
	}
}

func TestOSSRejectsOversizedDataObjectKeyBeforeCreatingState(t *testing.T) {
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	backend := newFakeBackend()
	cfg := testOSSConfig(db)
	cfg.Prefix = strings.Repeat("p", maxOSSObjectKeyBytes)
	sink := newWithBackend(cfg, db, backend)
	batch, _ := testOSSBatch()
	err = sink.Consume(context.Background(), batch)
	if err == nil || api.IsRetryableError(err) || !strings.Contains(err.Error(), "object key") {
		t.Fatalf("error=%v retryable=%v", err, api.IsRetryableError(err))
	}
	if _, found, stateErr := db.GetSinkStream(name, batch.StreamRef.ID); stateErr != nil || found {
		t.Fatalf("found=%v state error=%v", found, stateErr)
	}
	if len(backend.objects) != 0 {
		t.Fatalf("oversized key created objects: %+v", backend.objects)
	}
}

func TestOSSRejectsObjectFamilyWhoseFutureMarkerKeyWouldBeOversized(t *testing.T) {
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	backend := newFakeBackend()
	batch, resource := testOSSBatch()
	dataSuffix := objectKey("", resource, ^uint64(0))
	cfg := testOSSConfig(db)
	cfg.Prefix = strings.Repeat("p", maxOSSObjectKeyBytes-len(dataSuffix)-1)
	if got := len(objectKey(cfg.Prefix, resource, ^uint64(0))); got != maxOSSObjectKeyBytes {
		t.Fatalf("maximum-generation data key length=%d", got)
	}
	if got := len(markerKey(cfg.Prefix, resource, ^uint64(0))); got <= maxOSSObjectKeyBytes {
		t.Fatalf("maximum-revision marker key length=%d", got)
	}
	sink := newWithBackend(cfg, db, backend)
	err = sink.Consume(context.Background(), batch)
	if err == nil || api.IsRetryableError(err) || !strings.Contains(err.Error(), "object key") {
		t.Fatalf("error=%v retryable=%v", err, api.IsRetryableError(err))
	}
	if _, found, stateErr := db.GetSinkStream(name, batch.StreamRef.ID); stateErr != nil || found {
		t.Fatalf("found=%v state error=%v", found, stateErr)
	}
	if len(backend.objects) != 0 {
		t.Fatalf("invalid object family created objects: %+v", backend.objects)
	}
}

func TestOSSObjectKeyValidationRejectsInvalidEncodingAndLeadingSeparators(t *testing.T) {
	for _, key := range []string{"/leading-slash", `\leading-backslash`, string([]byte{0xff})} {
		err := validateOSSObjectKey(key)
		if err == nil || api.IsRetryableError(err) {
			t.Fatalf("key=%q error=%v retryable=%v", key, err, api.IsRetryableError(err))
		}
	}
}

func TestOSSRejectsInconsistentBatchResources(t *testing.T) {
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	backend := newFakeBackend()
	sink := newWithBackend(testOSSConfig(db), db, backend)
	batch, _ := testOSSBatch()
	second := batch.Items[0]
	second.Record.Resource.PodUID = "other-pod"
	batch.Items = append(batch.Items, second)
	err = sink.Consume(context.Background(), batch)
	if err == nil || api.IsRetryableError(err) {
		t.Fatalf("error=%v retryable=%v", err, api.IsRetryableError(err))
	}
}

func TestOSSRejectsPersistedObjectKeyOutsideStreamLayout(t *testing.T) {
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	batch, _ := testOSSBatch()
	if err := db.PutSinkStream(name, state.SinkStream{StreamRef: batch.StreamRef.ID, ObjectKey: "logs/foreign/object.log"}); err != nil {
		t.Fatal(err)
	}
	sink := newWithBackend(testOSSConfig(db), db, newFakeBackend())
	err = sink.Consume(context.Background(), batch)
	if err == nil || api.IsRetryableError(err) || !strings.Contains(err.Error(), "object key") {
		t.Fatalf("error=%v retryable=%v", err, api.IsRetryableError(err))
	}
}

func TestOSSRejectsForeignMetadataAfterUnknownAppend(t *testing.T) {
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	backend := newFakeBackend()
	backend.appendResult = "foreign-same-size"
	sink := newWithBackend(testOSSConfig(db), db, backend)
	batch, _ := testOSSBatch()
	err = sink.Consume(context.Background(), batch)
	if err == nil || api.IsRetryableError(err) || !strings.Contains(err.Error(), "stream-ref") {
		t.Fatalf("error=%v retryable=%v", err, api.IsRetryableError(err))
	}
}

func TestOSSRetryAfterFinalCheckpointFailureUsesPersistedIntent(t *testing.T) {
	for _, test := range []struct {
		name        string
		changeBody  bool
		wantSuccess bool
	}{{name: "same batch", wantSuccess: true}, {name: "different same-size batch", changeBody: true}} {
		t.Run(test.name, func(t *testing.T) {
			db, err := state.Open(t.TempDir(), "target", 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			store := &failFinalCheckpointStore{DB: db, fail: true}
			backend := newFakeBackend()
			sink := newWithBackend(testOSSConfig(db), store, backend)
			batch, resource := testOSSBatch()
			if err := sink.Consume(context.Background(), batch); err == nil || !strings.Contains(err.Error(), "injected") {
				t.Fatalf("first Consume() error=%v", err)
			}
			original := append([]byte(nil), backend.objects[objectKey("logs", resource, 0)].data...)
			if test.changeBody {
				batch.Items[0].Record.Body = []byte("world")
			}
			err = sink.Consume(context.Background(), batch)
			if test.wantSuccess {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || api.IsRetryableError(err) || !strings.Contains(err.Error(), "persisted intent") {
				t.Fatalf("retry error=%v retryable=%v", err, api.IsRetryableError(err))
			}
			if got := backend.objects[objectKey("logs", resource, 0)].data; !bytes.Equal(got, original) {
				t.Fatalf("retry changed remote bytes: got=%q want=%q", got, original)
			}
		})
	}
}

func TestOSSRejectsResourceChangeAcrossBatches(t *testing.T) {
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	sink := newWithBackend(testOSSConfig(db), db, newFakeBackend())
	batch, _ := testOSSBatch()
	if err := sink.Consume(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
	batch.Items[0].Record.Resource.PodName = "different-pod"
	err = sink.Consume(context.Background(), batch)
	if err == nil || api.IsRetryableError(err) || !strings.Contains(err.Error(), "resource identity changed") {
		t.Fatalf("error=%v retryable=%v", err, api.IsRetryableError(err))
	}
}

func TestOSSRejectsUnsafeMetadataValue(t *testing.T) {
	db, err := state.Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	sink := newWithBackend(testOSSConfig(db), db, newFakeBackend())
	batch, _ := testOSSBatch()
	batch.Items[0].Record.Resource.NodeName = "node\r\ninjected: value"
	err = sink.Consume(context.Background(), batch)
	if err == nil || api.IsRetryableError(err) || !strings.Contains(err.Error(), "non-visible-ASCII") {
		t.Fatalf("error=%v retryable=%v", err, api.IsRetryableError(err))
	}
}

func testOSSConfig(db *state.DB) Config {
	return Config{Prefix: "logs", ClusterID: "prod-a", WriterID: db.WriterID(), TargetID: db.TargetID(), MaxObjectBytes: 1 << 20, Timeout: time.Second}
}

func testOSSBatch() (api.Batch, api.Resource) {
	resource := api.Resource{SandboxID: "sb", ClusterName: "prod-a", Namespace: "ns", PodName: "pod", PodUID: "uid", NodeName: "node", Container: "sandbox", LogDirectory: "/var/log/pods/ns_pod_uid/sandbox"}
	streamRef := api.StreamRef{ID: "container-logs/uid/sandbox"}
	batch := api.Batch{StreamRef: streamRef, Items: []api.BatchItem{{RecordID: "r", Record: api.Record{Kind: api.RecordKindContainerLog, Timestamp: time.Date(2026, 7, 23, 10, 0, 0, 0, time.UTC), Body: []byte("hello"), Resource: resource, Attributes: map[string]string{"stream": "stdout"}}}}}
	return batch, resource
}

func testOSSMetadata(cfg Config, streamRef api.StreamRef, resource api.Resource, generation uint64) map[string]string {
	return map[string]string{
		"nodeagent-writer-id":  cfg.WriterID,
		"nodeagent-target-id":  cfg.TargetID,
		"nodeagent-stream-ref": streamRef.ID,
		"nodeagent-generation": strconv.FormatUint(generation, 10),
		"sandbox-id":           resource.SandboxID,
		"k8s-cluster-name":     resource.ClusterName,
		"k8s-namespace-name":   resource.Namespace,
		"k8s-pod-name":         resource.PodName,
		"k8s-pod-uid":          resource.PodUID,
		"k8s-container-name":   resource.Container,
		"k8s-node-name":        resource.NodeName,
		"log-directory":        resource.LogDirectory,
	}
}

func int64Pointer(value int64) *int64 {
	return &value
}

func cloneMap(input map[string]string) map[string]string {
	if input == nil {
		return nil
	}
	out := make(map[string]string, len(input))
	for key, value := range input {
		out[key] = value
	}
	return out
}
