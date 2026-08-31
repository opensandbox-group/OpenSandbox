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
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/alibaba/opensandbox/nodeagent/pkg/api"
	"github.com/alibaba/opensandbox/nodeagent/pkg/config"
	"github.com/alibaba/opensandbox/nodeagent/pkg/identity"
	"github.com/alibaba/opensandbox/nodeagent/pkg/marker"
	"github.com/alibaba/opensandbox/nodeagent/pkg/registry"
	"github.com/alibaba/opensandbox/nodeagent/pkg/state"
	"github.com/alibaba/opensandbox/nodeagent/pkg/streamformat"
	aliyunoss "github.com/aliyun/aliyun-oss-go-sdk/oss"
)

const (
	name                 = "oss"
	streamLockCount      = 64
	maxOSSObjectKeyBytes = 1023
	maxObjectBytes       = int64(1 << 30)
	ossObjectTypeHeader  = "X-Oss-Object-Type"
	ossSealedTimeHeader  = "X-Oss-Sealed-Time"
	appendableObjectType = "Appendable"
)

var errObjectNotFound = errors.New("OSS object not found")

func init() {
	registry.RegisterSink(name, func(cfg config.Config) (string, error) {
		return identity.OSSTargetID(cfg.OSSEndpoint, cfg.OSSBucket, cfg.OSSKeyPrefix, cfg.ClusterID)
	}, func(dependencies registry.SinkDependencies) (api.Sink, error) {
		cfg := dependencies.Config
		return newOSSSink(ossConfig{Endpoint: cfg.OSSEndpoint, Bucket: cfg.OSSBucket, Prefix: cfg.OSSKeyPrefix, ClusterID: cfg.ClusterID, AccessKeyID: cfg.OSSAccessKeyID, AccessKeySecret: cfg.OSSAccessKeySecret, SessionToken: cfg.OSSSessionToken, WriterID: dependencies.State.WriterID(), TargetID: dependencies.State.TargetID(), MaxObjectBytes: maxObjectBytes, Timeout: cfg.SinkTimeout}, dependencies.State)
	})
}

type stateStore interface {
	GetSinkStream(sinkName, streamRef string) (state.SinkStream, bool, error)
	PutSinkStream(sinkName string, stream state.SinkStream) error
}

type ossConfig struct {
	Endpoint        string
	Bucket          string
	Prefix          string
	ClusterID       string
	AccessKeyID     string
	AccessKeySecret string
	SessionToken    string
	WriterID        string
	TargetID        string
	MaxObjectBytes  int64
	Timeout         time.Duration
}

type ossSink struct {
	cfg     ossConfig
	backend backend
	state   stateStore

	cacheMu     sync.Mutex
	streamLocks [streamLockCount]sync.Mutex
	streams     map[string]cachedStream
}

type cachedStream struct {
	stream   state.SinkStream
	ref      api.StreamRef
	resource api.Resource
	metadata api.StreamMetadata
}

type objectMetadata struct {
	Size               int64
	CRC64              string
	Metadata           map[string]string
	ObjectType         string
	NextAppendPosition *int64
	SealedTime         string
}

type backend interface {
	Preflight(context.Context, string) error
	Append(context.Context, string, []byte, int64, string, map[string]string) (int64, error)
	Head(context.Context, string) (objectMetadata, error)
	PutMarker(context.Context, string, []byte) error
	Get(context.Context, string) ([]byte, error)
}

type realBackend struct {
	client     *aliyunoss.Client
	bucket     *aliyunoss.Bucket
	bucketName string
}

func newOSSSink(cfg ossConfig, store stateStore) (*ossSink, error) {
	if cfg.MaxObjectBytes <= 0 {
		return nil, errors.New("OSS object limit must be positive")
	}
	seconds := int64(cfg.Timeout.Seconds())
	if seconds < 1 {
		seconds = 1
	}
	opts := []aliyunoss.ClientOption{aliyunoss.Timeout(seconds, seconds)}
	if cfg.SessionToken != "" {
		opts = append(opts, aliyunoss.SecurityToken(cfg.SessionToken))
	}
	client, err := aliyunoss.New(cfg.Endpoint, cfg.AccessKeyID, cfg.AccessKeySecret, opts...)
	if err != nil {
		return nil, err
	}
	bucket, err := client.Bucket(cfg.Bucket)
	if err != nil {
		return nil, err
	}
	sink := newWithBackend(cfg, store, &realBackend{client: client, bucket: bucket, bucketName: cfg.Bucket})
	if err := sink.preflight(context.Background()); err != nil {
		return nil, err
	}
	return sink, nil
}

func newWithBackend(cfg ossConfig, store stateStore, storage backend) *ossSink {
	return &ossSink{cfg: cfg, backend: storage, state: store, streams: make(map[string]cachedStream)}
}

func (s *ossSink) Capabilities() api.Capabilities {
	return api.Capabilities{RecordKinds: streamformat.Kinds()}
}
func (s *ossSink) Guarantee() api.DeliveryGuarantee { return api.GuaranteeDurable }

func (s *ossSink) preflight(ctx context.Context) error {
	managed := strings.Trim(path.Join(s.cfg.Prefix, s.cfg.ClusterID), "/") + "/"
	return classifyOSSError(s.backend.Preflight(ctx, managed))
}

func (b *realBackend) Preflight(ctx context.Context, managed string) error {
	versioning, err := b.client.GetBucketVersioning(b.bucketName, aliyunoss.WithContext(ctx))
	if err != nil {
		return fmt.Errorf("read OSS bucket versioning: %w", err)
	}
	if versioning.Status != "" {
		return api.Permanent(fmt.Errorf("OSS bucket versioning must be disabled, got %q", versioning.Status))
	}
	if worm, err := b.client.GetBucketWorm(b.bucketName, aliyunoss.WithContext(ctx)); err == nil {
		if worm.WormId != "" || worm.State != "" {
			return api.Permanent(errors.New("OSS bucket WORM must not be configured"))
		}
	} else if !serviceCode(err, "NoSuchWORMConfiguration") && !serviceCode(err, "WormConfigurationNotFoundError") {
		return fmt.Errorf("read OSS bucket WORM: %w", err)
	}
	lifecycle, err := b.client.GetBucketLifecycle(b.bucketName, aliyunoss.WithContext(ctx))
	if err != nil {
		if !serviceCode(err, "NoSuchLifecycle") {
			return fmt.Errorf("read OSS lifecycle: %w", err)
		}
		return nil
	}
	for _, rule := range lifecycle.Rules {
		prefix := strings.Trim(rule.Prefix, "/")
		if prefix == "" || strings.HasPrefix(managed, prefix+"/") || strings.HasPrefix(prefix+"/", managed) {
			return api.Permanent(fmt.Errorf("OSS lifecycle prefix %q overlaps managed prefix %q", rule.Prefix, managed))
		}
	}
	return nil
}

func (b *realBackend) Append(ctx context.Context, key string, data []byte, position int64, contentType string, metadata map[string]string) (int64, error) {
	options := []aliyunoss.Option{aliyunoss.ContentType(contentType), aliyunoss.WithContext(ctx)}
	for key, value := range metadata {
		options = append(options, aliyunoss.Meta(key, value))
	}
	return b.bucket.AppendObject(key, bytes.NewReader(data), position, options...)
}

func (b *realBackend) Head(ctx context.Context, key string) (objectMetadata, error) {
	header, err := b.bucket.GetObjectDetailedMeta(key, aliyunoss.WithContext(ctx))
	if err != nil {
		return objectMetadata{}, err
	}
	return parseObjectMetadata(header)
}

func parseObjectMetadata(header http.Header) (objectMetadata, error) {
	size, err := strconv.ParseInt(header.Get("Content-Length"), 10, 64)
	if err != nil || size < 0 {
		return objectMetadata{}, api.Permanent(fmt.Errorf("invalid OSS Content-Length header %q", header.Get("Content-Length")))
	}
	metadata := make(map[string]string)
	metadataPrefix := strings.ToLower(aliyunoss.HTTPHeaderOssMetaPrefix)
	for key := range header {
		lowerKey := strings.ToLower(key)
		if strings.HasPrefix(lowerKey, metadataPrefix) {
			metadata[strings.TrimPrefix(lowerKey, metadataPrefix)] = header.Get(key)
		}
	}
	var nextAppendPosition *int64
	if raw := header.Get(aliyunoss.HTTPHeaderOssNextAppendPosition); raw != "" {
		next, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || next < 0 {
			return objectMetadata{}, api.Permanent(fmt.Errorf("invalid OSS next append position header %q", raw))
		}
		nextAppendPosition = &next
	}
	return objectMetadata{
		Size:               size,
		CRC64:              header.Get(aliyunoss.HTTPHeaderOssCRC64),
		Metadata:           metadata,
		ObjectType:         header.Get(ossObjectTypeHeader),
		NextAppendPosition: nextAppendPosition,
		SealedTime:         header.Get(ossSealedTimeHeader),
	}, nil
}

func (b *realBackend) PutMarker(ctx context.Context, key string, data []byte) error {
	return b.bucket.PutObject(key, bytes.NewReader(data), aliyunoss.ContentType("application/json"), aliyunoss.ForbidOverWrite(true), aliyunoss.WithContext(ctx))
}

func (b *realBackend) Get(ctx context.Context, key string) ([]byte, error) {
	reader, err := b.bucket.GetObject(key, aliyunoss.WithContext(ctx))
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	return io.ReadAll(reader)
}

func (s *ossSink) Consume(ctx context.Context, batch api.Batch) error {
	if len(batch.Items) == 0 {
		return nil
	}
	format, resource, data, err := streamformat.EncodeBatch(batch)
	if err != nil {
		return api.Permanent(fmt.Errorf("encode OSS batch: %w", err))
	}
	if int64(len(data)) > s.cfg.MaxObjectBytes {
		return api.Permanent(fmt.Errorf("encoded batch size %d exceeds per-generation limit %d", len(data), s.cfg.MaxObjectBytes))
	}
	streamLock := s.streamLock(batch.StreamRef.ID)
	streamLock.Lock()
	defer streamLock.Unlock()
	if err := s.validateResource(batch.StreamRef, resource, batch.Metadata, format); err != nil {
		return err
	}
	stream, err := s.getStream(ctx, batch.StreamRef, resource, batch.Metadata, format)
	if err != nil {
		return err
	}
	if stream.CurrentClosed {
		stream.Generation++
		stream.Position = 0
		stream.CurrentClosed = false
		stream.ObjectKey, err = dataKey(format, s.cfg.Prefix, batch.StreamRef, resource, batch.Metadata, stream.Generation)
		if err != nil {
			return err
		}
		if err := validateOSSObjectKey(stream.ObjectKey); err != nil {
			return err
		}
	}
	if appendExceedsObjectLimit(stream.Position, int64(len(data)), s.cfg.MaxObjectBytes) {
		if err := s.closeGeneration(ctx, batch.StreamRef, resource, batch.Metadata, format, &stream); err != nil {
			return err
		}
		stream.Generation++
		stream.Position = 0
		stream.CurrentClosed = false
		stream.ObjectKey, err = dataKey(format, s.cfg.Prefix, batch.StreamRef, resource, batch.Metadata, stream.Generation)
		if err != nil {
			return err
		}
		if err := validateOSSObjectKey(stream.ObjectKey); err != nil {
			return err
		}
	}
	digest := sha256.Sum256(data)
	intent := state.AppendIntent{Position: stream.Position, Length: int64(len(data)), SHA256: hex.EncodeToString(digest[:])}
	if stream.AppendIntent != nil && *stream.AppendIntent != intent {
		return api.Permanent(errors.New("OSS append retry does not match persisted intent"))
	}
	stream.AppendIntent = &intent
	if err := s.state.PutSinkStream(name, stream); err != nil {
		return err
	}
	s.storeCachedStream(batch.StreamRef, stream, resource, batch.Metadata)
	metadata := map[string]string(nil)
	if stream.Position == 0 {
		metadata, err = s.objectIdentityMetadata(batch.StreamRef, resource, batch.Metadata, format, stream.Generation)
		if err != nil {
			return err
		}
		if metadataBytes(metadata) > 8<<10 {
			return api.Permanent(errors.New("OSS object metadata exceeds 8 KiB"))
		}
	}
	next, appendErr := s.backend.Append(ctx, stream.ObjectKey, data, stream.Position, format.ContentType(), metadata)
	appendErr = classifyOSSError(appendErr)
	expected := stream.Position + int64(len(data))
	if appendErr != nil || next != expected {
		next, err = s.recoverAppendResult(ctx, batch.StreamRef, resource, batch.Metadata, format, stream, next, expected, appendErr)
		if err != nil {
			return err
		}
	}
	stream.Position = next
	stream.AppendIntent = nil
	if err := s.state.PutSinkStream(name, stream); err != nil {
		return err
	}
	s.storeCachedStream(batch.StreamRef, stream, resource, batch.Metadata)
	return nil
}

func (s *ossSink) recoverAppendResult(ctx context.Context, streamRef api.StreamRef, resource api.Resource, streamMetadata api.StreamMetadata, format streamformat.Format, stream state.SinkStream, next, expected int64, appendErr error) (int64, error) {
	metadata, headErr := s.backend.Head(ctx, stream.ObjectKey)
	headErr = classifyOSSError(headErr)
	if headErr == nil {
		if err := s.validateObjectIdentity(metadata, stream, streamRef, resource, streamMetadata, format); err != nil {
			s.deleteCachedStream(streamRef.ID)
			return 0, err
		}
		switch metadata.Size {
		case expected:
			return expected, nil
		case stream.Position:
			s.deleteCachedStream(streamRef.ID)
			if appendErr != nil {
				return 0, fmt.Errorf("OSS append result unknown at position %d: %w", stream.Position, appendErr)
			}
			return 0, fmt.Errorf("OSS append returned next position %d while the object remained at %d", next, stream.Position)
		default:
			s.deleteCachedStream(streamRef.ID)
			return 0, api.Permanent(fmt.Errorf("OSS append position conflict: remote position %d, committed position %d, expected position %d", metadata.Size, stream.Position, expected))
		}
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	s.deleteCachedStream(streamRef.ID)
	if isNotFound(headErr) && stream.Position == 0 {
		if appendErr != nil {
			return 0, fmt.Errorf("OSS append result unknown at position 0: %w", appendErr)
		}
		return 0, fmt.Errorf("OSS append returned next position %d but the object was not created", next)
	}
	if appendErr != nil {
		return 0, errors.Join(
			fmt.Errorf("OSS append result unknown at position %d: %w", stream.Position, appendErr),
			fmt.Errorf("OSS HeadObject after append failed: %w", headErr),
		)
	}
	return 0, fmt.Errorf("OSS append returned next position %d; HeadObject failed: %w", next, headErr)
}

func (s *ossSink) Finalize(ctx context.Context, request api.FinalizeRequest) error {
	format, err := streamformat.Lookup(request.StreamRef.Kind)
	if err != nil {
		return api.Permanent(fmt.Errorf("finalize OSS stream: %w", err))
	}
	streamLock := s.streamLock(request.StreamRef.ID)
	streamLock.Lock()
	defer streamLock.Unlock()
	if err := s.validateResource(request.StreamRef, request.Resource, request.Metadata, format); err != nil {
		return err
	}
	key, err := finalizationKey(format, s.cfg.Prefix, request.StreamRef, request.Resource, request.Metadata, request.Revision)
	if err != nil {
		return err
	}
	if err := validateOSSObjectKey(key); err != nil {
		return err
	}
	if err := s.preflight(ctx); err != nil {
		return err
	}
	stream, err := s.getStream(ctx, request.StreamRef, request.Resource, request.Metadata, format)
	if err != nil {
		return err
	}
	if stream.Position > 0 {
		if err := s.closeGeneration(ctx, request.StreamRef, request.Resource, request.Metadata, format, &stream); err != nil {
			return err
		}
	}
	if err := s.verifyClosedObjects(ctx, request.StreamRef, request.Resource, request.Metadata, format, stream.ClosedObjects); err != nil {
		return err
	}
	if request.Revision < stream.FinalizedRevision || request.Revision > stream.FinalizedRevision+1 {
		return api.Permanent(fmt.Errorf("OSS marker revision %d is not continuous after %d", request.Revision, stream.FinalizedRevision))
	}
	raw, err := marker.Encode(marker.New(request, stream.ClosedObjects))
	if err != nil {
		return api.Permanent(err)
	}
	err = classifyOSSError(s.backend.PutMarker(ctx, key, raw))
	if err == nil {
		stream.FinalizedRevision = request.Revision
		if err := s.state.PutSinkStream(name, stream); err != nil {
			return err
		}
		s.deleteCachedStream(request.StreamRef.ID)
		return nil
	}
	existing, getErr := s.backend.Get(ctx, key)
	getErr = classifyOSSError(getErr)
	if getErr != nil {
		return errors.Join(err, getErr)
	}
	if !bytes.Equal(existing, raw) {
		return api.Permanent(errors.New("conflicting OSS finalization marker"))
	}
	stream.FinalizedRevision = request.Revision
	if err := s.state.PutSinkStream(name, stream); err != nil {
		return err
	}
	s.deleteCachedStream(request.StreamRef.ID)
	return nil
}

func (s *ossSink) Close(context.Context) error { return nil }

func (s *ossSink) getStream(ctx context.Context, streamRef api.StreamRef, resource api.Resource, streamMetadata api.StreamMetadata, format streamformat.Format) (state.SinkStream, error) {
	if current, cachedRef, cachedResource, cachedMetadata, ok := s.cachedStream(streamRef.ID); ok {
		if cachedRef != streamRef || cachedResource != resource || !cachedMetadata.Equal(streamMetadata) {
			return state.SinkStream{}, api.Permanent(errors.New("OSS stream identity or metadata changed"))
		}
		return current, nil
	}
	stream, found, err := s.state.GetSinkStream(name, streamRef.ID)
	if err != nil {
		return state.SinkStream{}, err
	}
	if !found {
		objectKey, err := dataKey(format, s.cfg.Prefix, streamRef, resource, streamMetadata, 0)
		if err != nil {
			return state.SinkStream{}, err
		}
		stream = state.SinkStream{SinkName: name, StreamRef: streamRef.ID, ObjectKey: objectKey}
		if err := validateOSSObjectKey(stream.ObjectKey); err != nil {
			return state.SinkStream{}, err
		}
		if _, err := s.backend.Head(ctx, stream.ObjectKey); err == nil {
			return state.SinkStream{}, api.Permanent(errors.New("refusing to adopt existing OSS object without state"))
		} else if !isNotFound(err) {
			return state.SinkStream{}, classifyOSSError(err)
		}
	} else {
		if err := s.validateStreamLayout(stream, streamRef, resource, streamMetadata, format); err != nil {
			return state.SinkStream{}, err
		}
		if stream.ObjectKey == "" {
			stream.ObjectKey, err = dataKey(format, s.cfg.Prefix, streamRef, resource, streamMetadata, stream.Generation)
			if err != nil {
				return state.SinkStream{}, err
			}
		}
		if stream.AppendIntent != nil {
			size, err := s.objectSize(ctx, stream.ObjectKey)
			if err != nil && isNotFound(err) && stream.AppendIntent.Position == 0 {
				size = 0
			} else if err != nil {
				if isNotFound(err) {
					return state.SinkStream{}, api.Permanent(fmt.Errorf("OSS append target %q is missing for persisted position %d: %w", stream.ObjectKey, stream.AppendIntent.Position, err))
				}
				return state.SinkStream{}, err
			}
			intent := stream.AppendIntent
			switch size {
			case intent.Position:
				stream.AppendIntent = nil
			case intent.Position + intent.Length:
				// The Source checkpoint was not committed, so replay is allowed.
				stream.Position = size
				stream.AppendIntent = nil
			default:
				return state.SinkStream{}, api.Permanent(fmt.Errorf("OSS append intent position conflict: %d", size))
			}
		}
		if err := s.verifyMetadata(ctx, stream, streamRef, resource, streamMetadata, format); err != nil {
			return state.SinkStream{}, err
		}
	}
	if err := s.state.PutSinkStream(name, stream); err != nil {
		return state.SinkStream{}, err
	}
	s.storeCachedStream(streamRef, stream, resource, streamMetadata)
	return stream, nil
}

func (s *ossSink) closeGeneration(ctx context.Context, streamRef api.StreamRef, resource api.Resource, streamMetadata api.StreamMetadata, format streamformat.Format, stream *state.SinkStream) error {
	metadata, err := s.backend.Head(ctx, stream.ObjectKey)
	if err != nil {
		return existingObjectError("close generation", stream.ObjectKey, err)
	}
	if err := s.validateObjectIdentity(metadata, *stream, streamRef, resource, streamMetadata, format); err != nil {
		return err
	}
	size := metadata.Size
	if size != stream.Position {
		return api.Permanent(fmt.Errorf("OSS object size %d does not match position %d", size, stream.Position))
	}
	crc := metadata.CRC64
	if crc == "" {
		return api.Permanent(errors.New("OSS object CRC64 header missing"))
	}
	object := state.ClosedObject{Key: stream.ObjectKey, Generation: stream.Generation, Size: size, CRC64: crc}
	if len(stream.ClosedObjects) == 0 || stream.ClosedObjects[len(stream.ClosedObjects)-1].Generation != object.Generation {
		stream.ClosedObjects = append(stream.ClosedObjects, object)
	}
	stream.CurrentClosed = true
	if err := s.state.PutSinkStream(name, *stream); err != nil {
		return err
	}
	s.storeCachedStream(streamRef, *stream, resource, streamMetadata)
	return nil
}

func (s *ossSink) verifyMetadata(ctx context.Context, stream state.SinkStream, streamRef api.StreamRef, resource api.Resource, streamMetadata api.StreamMetadata, format streamformat.Format) error {
	metadata, err := s.backend.Head(ctx, stream.ObjectKey)
	if err != nil {
		if isNotFound(err) && stream.Position == 0 {
			return nil
		}
		return existingObjectError("verify checkpoint", stream.ObjectKey, err)
	}
	if err := s.validateObjectIdentity(metadata, stream, streamRef, resource, streamMetadata, format); err != nil {
		return err
	}
	if metadata.Size != stream.Position {
		return api.Permanent(errors.New("OSS object position does not match local state"))
	}
	return nil
}

func (s *ossSink) objectSize(ctx context.Context, key string) (int64, error) {
	metadata, err := s.backend.Head(ctx, key)
	if err != nil {
		return 0, classifyOSSError(err)
	}
	return metadata.Size, nil
}

func (s *ossSink) verifyClosedObjects(ctx context.Context, streamRef api.StreamRef, resource api.Resource, streamMetadata api.StreamMetadata, format streamformat.Format, objects []state.ClosedObject) error {
	for index, object := range objects {
		if err := validateOSSObjectKey(object.Key); err != nil {
			return err
		}
		expectedKey, err := dataKey(format, s.cfg.Prefix, streamRef, resource, streamMetadata, object.Generation)
		if err != nil {
			return err
		}
		if object.Generation != uint64(index) || object.Key != expectedKey {
			return api.Permanent(fmt.Errorf("OSS closed generation %d has an invalid object layout", object.Generation))
		}
		metadata, err := s.backend.Head(ctx, object.Key)
		if err != nil {
			return existingObjectError("verify closed generation", object.Key, err)
		}
		if metadata.Size != object.Size || metadata.CRC64 != object.CRC64 {
			return api.Permanent(fmt.Errorf("OSS object %s changed after logical close", object.Key))
		}
		stream := state.SinkStream{SinkName: name, StreamRef: streamRef.ID, Generation: object.Generation}
		if err := s.validateObjectIdentity(metadata, stream, streamRef, resource, streamMetadata, format); err != nil {
			return err
		}
	}
	return nil
}

func metadataBytes(metadata map[string]string) int {
	total := 0
	for key, value := range metadata {
		total += len(aliyunoss.HTTPHeaderOssMetaPrefix) + len(key) + len(value)
	}
	return total
}

func appendExceedsObjectLimit(position, appendBytes, limit int64) bool {
	return position > limit || appendBytes > limit-position
}

func dataKey(format streamformat.Format, prefix string, streamRef api.StreamRef, resource api.Resource, metadata api.StreamMetadata, generation uint64) (string, error) {
	family, err := streamformat.ResolveFamily(format, prefix, streamRef, resource, metadata)
	if err != nil {
		return "", api.Permanent(fmt.Errorf("resolve OSS object family: %w", err))
	}
	return family.DataKey(generation), nil
}

func finalizationKey(format streamformat.Format, prefix string, streamRef api.StreamRef, resource api.Resource, metadata api.StreamMetadata, revision uint64) (string, error) {
	family, err := streamformat.ResolveFamily(format, prefix, streamRef, resource, metadata)
	if err != nil {
		return "", api.Permanent(fmt.Errorf("resolve OSS object family: %w", err))
	}
	return family.MarkerKey(revision), nil
}

func serviceCode(err error, code string) bool {
	var serviceErr aliyunoss.ServiceError
	return errors.As(err, &serviceErr) && serviceErr.Code == code
}

func isNotFound(err error) bool {
	if errors.Is(err, errObjectNotFound) {
		return true
	}
	var serviceErr aliyunoss.ServiceError
	if !errors.As(err, &serviceErr) {
		return false
	}
	// HEAD responses have no XML body. When OSS also omits X-Oss-Err, the SDK
	// can only preserve the 404 status; coded 404s such as NoSuchBucket remain
	// distinguishable and must not be treated as a missing object.
	return serviceErr.Code == "NoSuchKey" || serviceErr.StatusCode == http.StatusNotFound && serviceErr.Code == ""
}

func (s *ossSink) streamLock(streamRef string) *sync.Mutex {
	hash := uint32(2166136261)
	for i := 0; i < len(streamRef); i++ {
		hash ^= uint32(streamRef[i])
		hash *= 16777619
	}
	return &s.streamLocks[hash%streamLockCount]
}

func (s *ossSink) validateResource(streamRef api.StreamRef, resource api.Resource, streamMetadata api.StreamMetadata, format streamformat.Format) error {
	if resource.ClusterName != s.cfg.ClusterID {
		return api.Permanent(fmt.Errorf("resource cluster %q does not match configured cluster %q", resource.ClusterName, s.cfg.ClusterID))
	}
	if err := streamMetadata.Validate(); err != nil {
		return api.Permanent(fmt.Errorf("invalid stream metadata: %w", err))
	}
	family, err := streamformat.ResolveFamily(format, s.cfg.Prefix, streamRef, resource, streamMetadata)
	if err != nil {
		return api.Permanent(fmt.Errorf("resolve OSS object family: %w", err))
	}
	for _, field := range []struct{ name, value string }{
		{name: "sandbox ID", value: resource.SandboxID},
		{name: "cluster name", value: resource.ClusterName},
		{name: "namespace", value: resource.Namespace},
		{name: "Pod name", value: resource.PodName},
		{name: "Pod UID", value: resource.PodUID},
		{name: "node name", value: resource.NodeName},
		{name: "container name", value: resource.Container},
	} {
		name, value := field.name, field.value
		if value == "" {
			return api.Permanent(fmt.Errorf("OSS metadata resource field %s is empty", name))
		}
		for i := 0; i < len(value); i++ {
			if value[i] < 0x20 || value[i] > 0x7e {
				return api.Permanent(fmt.Errorf("OSS metadata resource field %s contains a non-visible-ASCII byte", name))
			}
		}
	}
	formatMetadata, err := format.ObjectMetadata(resource, streamMetadata)
	if err != nil {
		return api.Permanent(fmt.Errorf("build OSS format metadata: %w", err))
	}
	if err := validateFormatMetadata(formatMetadata); err != nil {
		return api.Permanent(err)
	}
	for _, key := range []string{
		family.DataKey(^uint64(0)),
		family.MarkerKey(^uint64(0)),
	} {
		if err := validateOSSObjectKey(key); err != nil {
			return err
		}
	}
	return nil
}

func (s *ossSink) validateStreamLayout(stream state.SinkStream, streamRef api.StreamRef, resource api.Resource, streamMetadata api.StreamMetadata, format streamformat.Format) error {
	if stream.StreamRef != streamRef.ID {
		return api.Permanent(fmt.Errorf("OSS checkpoint stream %q does not match requested stream %q", stream.StreamRef, streamRef.ID))
	}
	if stream.Position < 0 {
		return api.Permanent(fmt.Errorf("OSS checkpoint position %d is negative", stream.Position))
	}
	if stream.Device != 0 || stream.Inode != 0 || len(stream.CRC64State) != 0 || stream.GenerationTransition != nil || stream.MarkerIntent != nil || stream.CleanupPhase != "" || stream.CleanupPath != "" {
		return api.Permanent(errors.New("OSS checkpoint contains file-sink-only state"))
	}
	if stream.AppendIntent != nil {
		intent := stream.AppendIntent
		digest, err := hex.DecodeString(intent.SHA256)
		if intent.Position != stream.Position || intent.Length <= 0 || intent.Position > (1<<63-1)-intent.Length || len(digest) != sha256.Size || err != nil || hex.EncodeToString(digest) != intent.SHA256 {
			return api.Permanent(errors.New("OSS checkpoint has an invalid append intent"))
		}
	}
	expected, err := dataKey(format, s.cfg.Prefix, streamRef, resource, streamMetadata, stream.Generation)
	if err != nil {
		return err
	}
	if err := validateOSSObjectKey(expected); err != nil {
		return err
	}
	unset := stream.ObjectKey == "" && stream.Position == 0 && stream.AppendIntent == nil && len(stream.ClosedObjects) == 0
	if !unset && stream.ObjectKey != expected {
		return api.Permanent(fmt.Errorf("OSS checkpoint object key %q does not match generation %d", stream.ObjectKey, stream.Generation))
	}
	expectedClosed := stream.Generation
	if stream.CurrentClosed {
		if expectedClosed == ^uint64(0) {
			return api.Permanent(errors.New("OSS checkpoint generation overflows closed-object count"))
		}
		expectedClosed++
	}
	if uint64(len(stream.ClosedObjects)) != expectedClosed {
		return api.Permanent(fmt.Errorf("OSS checkpoint has %d closed objects for generation %d (closed=%t)", len(stream.ClosedObjects), stream.Generation, stream.CurrentClosed))
	}
	if stream.CurrentClosed && stream.AppendIntent != nil {
		return api.Permanent(errors.New("closed OSS generation has an unresolved append intent"))
	}
	for index, object := range stream.ClosedObjects {
		expectedKey, err := dataKey(format, s.cfg.Prefix, streamRef, resource, streamMetadata, object.Generation)
		if err != nil {
			return err
		}
		if object.Generation != uint64(index) || object.Key != expectedKey {
			return api.Permanent(fmt.Errorf("OSS checkpoint closed generation %d has an invalid object layout", object.Generation))
		}
	}
	if stream.CurrentClosed {
		current := stream.ClosedObjects[len(stream.ClosedObjects)-1]
		if current.Key != stream.ObjectKey || current.Size != stream.Position {
			return api.Permanent(errors.New("closed OSS checkpoint does not match the current generation"))
		}
	}
	return nil
}

func existingObjectError(operation, key string, err error) error {
	err = classifyOSSError(err)
	if isNotFound(err) {
		return api.Permanent(fmt.Errorf("%s: OSS object %q is missing: %w", operation, key, err))
	}
	return err
}

func (s *ossSink) validateObjectIdentity(metadata objectMetadata, stream state.SinkStream, streamRef api.StreamRef, resource api.Resource, streamMetadata api.StreamMetadata, format streamformat.Format) error {
	if metadata.ObjectType != appendableObjectType {
		return api.Permanent(fmt.Errorf("OSS object type %q is not Appendable", metadata.ObjectType))
	}
	if metadata.SealedTime != "" {
		return api.Permanent(errors.New("OSS appendable object is sealed"))
	}
	if metadata.NextAppendPosition == nil {
		return api.Permanent(errors.New("OSS appendable object is missing its next append position"))
	}
	if *metadata.NextAppendPosition != metadata.Size {
		return api.Permanent(fmt.Errorf("OSS next append position %d does not match object size %d", *metadata.NextAppendPosition, metadata.Size))
	}
	expected, err := s.objectIdentityMetadata(streamRef, resource, streamMetadata, format, stream.Generation)
	if err != nil {
		return err
	}
	for key, value := range expected {
		if metadata.Metadata[key] != value {
			return api.Permanent(fmt.Errorf("OSS object metadata %s does not match stream identity", key))
		}
	}
	return nil
}

func (s *ossSink) objectIdentityMetadata(streamRef api.StreamRef, resource api.Resource, streamMetadata api.StreamMetadata, format streamformat.Format, generation uint64) (map[string]string, error) {
	expected := map[string]string{
		"nodeagent-writer-id":  s.cfg.WriterID,
		"nodeagent-target-id":  s.cfg.TargetID,
		"nodeagent-stream-ref": streamRef.ID,
		"nodeagent-generation": strconv.FormatUint(generation, 10),
		"sandbox-id":           resource.SandboxID,
		"k8s-cluster-name":     resource.ClusterName,
		"k8s-namespace-name":   resource.Namespace,
		"k8s-pod-name":         resource.PodName,
		"k8s-pod-uid":          resource.PodUID,
		"k8s-container-name":   resource.Container,
		"k8s-node-name":        resource.NodeName,
	}
	formatMetadata, err := format.ObjectMetadata(resource, streamMetadata)
	if err != nil {
		return nil, api.Permanent(fmt.Errorf("build OSS format metadata: %w", err))
	}
	if err := validateFormatMetadata(formatMetadata); err != nil {
		return nil, api.Permanent(err)
	}
	for key, value := range formatMetadata {
		if _, reserved := expected[key]; reserved {
			return nil, api.Permanent(fmt.Errorf("stream format metadata overrides reserved key %q", key))
		}
		expected[key] = value
	}
	return expected, nil
}

func validateFormatMetadata(metadata map[string]string) error {
	for key, value := range metadata {
		if key == "" || strings.HasPrefix(key, "x-oss-") {
			return fmt.Errorf("invalid stream format metadata key %q", key)
		}
		for i := 0; i < len(key); i++ {
			if (key[i] < 'a' || key[i] > 'z') && (key[i] < '0' || key[i] > '9') && key[i] != '-' {
				return fmt.Errorf("invalid stream format metadata key %q", key)
			}
		}
		for i := 0; i < len(value); i++ {
			if value[i] < 0x20 || value[i] > 0x7e {
				return fmt.Errorf("stream format metadata %q contains a non-visible-ASCII byte", key)
			}
		}
	}
	return nil
}

func (s *ossSink) cachedStream(streamRef string) (state.SinkStream, api.StreamRef, api.Resource, api.StreamMetadata, bool) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	entry, found := s.streams[streamRef]
	return entry.stream, entry.ref, entry.resource, entry.metadata, found
}

func (s *ossSink) storeCachedStream(streamRef api.StreamRef, stream state.SinkStream, resource api.Resource, metadata api.StreamMetadata) {
	s.cacheMu.Lock()
	s.streams[streamRef.ID] = cachedStream{stream: stream, ref: streamRef, resource: resource, metadata: metadata.Clone()}
	s.cacheMu.Unlock()
}

func (s *ossSink) deleteCachedStream(streamRef string) {
	s.cacheMu.Lock()
	delete(s.streams, streamRef)
	s.cacheMu.Unlock()
}

func validateOSSObjectKey(key string) error {
	if !utf8.ValidString(key) {
		return api.Permanent(errors.New("OSS object key must be valid UTF-8"))
	}
	if strings.HasPrefix(key, "/") || strings.HasPrefix(key, `\`) {
		return api.Permanent(errors.New("OSS object key must not start with a slash or backslash"))
	}
	if len(key) == 0 || len(key) > maxOSSObjectKeyBytes {
		return api.Permanent(fmt.Errorf("OSS object key must contain 1 to %d UTF-8 bytes, got %d", maxOSSObjectKeyBytes, len(key)))
	}
	return nil
}

func classifyOSSError(err error) error {
	if err == nil {
		return nil
	}
	var serviceErr aliyunoss.ServiceError
	if errors.As(err, &serviceErr) {
		switch serviceErr.Code {
		case "AccessDenied",
			"AppendSealedObjectNotAllowed",
			"EntityTooLarge",
			"EntityTooSmall",
			"FileImmutable",
			"InvalidAccessKeyId",
			"InvalidArgument",
			"InvalidBucketName",
			"InvalidObjectName",
			"InvalidSecurityToken",
			"InvalidURI",
			"KmsServiceNotEnabled",
			"MalformedXML",
			"MethodNotAllowed",
			"NoSuchBucket",
			"NotImplemented",
			"ObjectNotAppendable",
			"SecurityTokenExpired",
			"SignatureDoesNotMatch":
			return api.Permanent(err)
		}
	}
	return err
}
