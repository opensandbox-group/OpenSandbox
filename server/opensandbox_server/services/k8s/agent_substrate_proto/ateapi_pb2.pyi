import datetime

from google.protobuf import timestamp_pb2 as _timestamp_pb2
from google.protobuf.internal import containers as _containers
from google.protobuf.internal import enum_type_wrapper as _enum_type_wrapper
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Iterable as _Iterable, Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class ExternalSnapshotInfo(_message.Message):
    __slots__ = ("snapshot_uri_prefix",)
    SNAPSHOT_URI_PREFIX_FIELD_NUMBER: _ClassVar[int]
    snapshot_uri_prefix: str
    def __init__(self, snapshot_uri_prefix: _Optional[str] = ...) -> None: ...

class LocalSnapshotInfo(_message.Message):
    __slots__ = ("snapshot_prefix", "node_vms_with_local_snapshots")
    SNAPSHOT_PREFIX_FIELD_NUMBER: _ClassVar[int]
    NODE_VMS_WITH_LOCAL_SNAPSHOTS_FIELD_NUMBER: _ClassVar[int]
    snapshot_prefix: str
    node_vms_with_local_snapshots: _containers.RepeatedScalarFieldContainer[str]
    def __init__(self, snapshot_prefix: _Optional[str] = ..., node_vms_with_local_snapshots: _Optional[_Iterable[str]] = ...) -> None: ...

class SnapshotInfo(_message.Message):
    __slots__ = ("external", "local")
    EXTERNAL_FIELD_NUMBER: _ClassVar[int]
    LOCAL_FIELD_NUMBER: _ClassVar[int]
    external: ExternalSnapshotInfo
    local: LocalSnapshotInfo
    def __init__(self, external: _Optional[_Union[ExternalSnapshotInfo, _Mapping]] = ..., local: _Optional[_Union[LocalSnapshotInfo, _Mapping]] = ...) -> None: ...

class Selector(_message.Message):
    __slots__ = ("match_labels",)
    class MatchLabelsEntry(_message.Message):
        __slots__ = ("key", "value")
        KEY_FIELD_NUMBER: _ClassVar[int]
        VALUE_FIELD_NUMBER: _ClassVar[int]
        key: str
        value: str
        def __init__(self, key: _Optional[str] = ..., value: _Optional[str] = ...) -> None: ...
    MATCH_LABELS_FIELD_NUMBER: _ClassVar[int]
    match_labels: _containers.ScalarMap[str, str]
    def __init__(self, match_labels: _Optional[_Mapping[str, str]] = ...) -> None: ...

class ResourceMetadata(_message.Message):
    __slots__ = ("atespace", "name", "uid", "version", "create_time", "update_time")
    ATESPACE_FIELD_NUMBER: _ClassVar[int]
    NAME_FIELD_NUMBER: _ClassVar[int]
    UID_FIELD_NUMBER: _ClassVar[int]
    VERSION_FIELD_NUMBER: _ClassVar[int]
    CREATE_TIME_FIELD_NUMBER: _ClassVar[int]
    UPDATE_TIME_FIELD_NUMBER: _ClassVar[int]
    atespace: str
    name: str
    uid: str
    version: int
    create_time: _timestamp_pb2.Timestamp
    update_time: _timestamp_pb2.Timestamp
    def __init__(self, atespace: _Optional[str] = ..., name: _Optional[str] = ..., uid: _Optional[str] = ..., version: _Optional[int] = ..., create_time: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., update_time: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ...) -> None: ...

class Actor(_message.Message):
    __slots__ = ("metadata", "actor_template_namespace", "actor_template_name", "status", "ateom_pod_namespace", "ateom_pod_name", "ateom_pod_ip", "in_progress_snapshot", "ateom_pod_uid", "latest_snapshot_info", "worker_selector", "worker_pool_name")
    class Status(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
        __slots__ = ()
        STATUS_UNSPECIFIED: _ClassVar[Actor.Status]
        STATUS_RESUMING: _ClassVar[Actor.Status]
        STATUS_RUNNING: _ClassVar[Actor.Status]
        STATUS_SUSPENDING: _ClassVar[Actor.Status]
        STATUS_SUSPENDED: _ClassVar[Actor.Status]
        STATUS_PAUSING: _ClassVar[Actor.Status]
        STATUS_PAUSED: _ClassVar[Actor.Status]
        STATUS_CRASHED: _ClassVar[Actor.Status]
    STATUS_UNSPECIFIED: Actor.Status
    STATUS_RESUMING: Actor.Status
    STATUS_RUNNING: Actor.Status
    STATUS_SUSPENDING: Actor.Status
    STATUS_SUSPENDED: Actor.Status
    STATUS_PAUSING: Actor.Status
    STATUS_PAUSED: Actor.Status
    STATUS_CRASHED: Actor.Status
    METADATA_FIELD_NUMBER: _ClassVar[int]
    ACTOR_TEMPLATE_NAMESPACE_FIELD_NUMBER: _ClassVar[int]
    ACTOR_TEMPLATE_NAME_FIELD_NUMBER: _ClassVar[int]
    STATUS_FIELD_NUMBER: _ClassVar[int]
    ATEOM_POD_NAMESPACE_FIELD_NUMBER: _ClassVar[int]
    ATEOM_POD_NAME_FIELD_NUMBER: _ClassVar[int]
    ATEOM_POD_IP_FIELD_NUMBER: _ClassVar[int]
    IN_PROGRESS_SNAPSHOT_FIELD_NUMBER: _ClassVar[int]
    ATEOM_POD_UID_FIELD_NUMBER: _ClassVar[int]
    LATEST_SNAPSHOT_INFO_FIELD_NUMBER: _ClassVar[int]
    WORKER_SELECTOR_FIELD_NUMBER: _ClassVar[int]
    WORKER_POOL_NAME_FIELD_NUMBER: _ClassVar[int]
    metadata: ResourceMetadata
    actor_template_namespace: str
    actor_template_name: str
    status: Actor.Status
    ateom_pod_namespace: str
    ateom_pod_name: str
    ateom_pod_ip: str
    in_progress_snapshot: str
    ateom_pod_uid: str
    latest_snapshot_info: SnapshotInfo
    worker_selector: Selector
    worker_pool_name: str
    def __init__(self, metadata: _Optional[_Union[ResourceMetadata, _Mapping]] = ..., actor_template_namespace: _Optional[str] = ..., actor_template_name: _Optional[str] = ..., status: _Optional[_Union[Actor.Status, str]] = ..., ateom_pod_namespace: _Optional[str] = ..., ateom_pod_name: _Optional[str] = ..., ateom_pod_ip: _Optional[str] = ..., in_progress_snapshot: _Optional[str] = ..., ateom_pod_uid: _Optional[str] = ..., latest_snapshot_info: _Optional[_Union[SnapshotInfo, _Mapping]] = ..., worker_selector: _Optional[_Union[Selector, _Mapping]] = ..., worker_pool_name: _Optional[str] = ...) -> None: ...

class Atespace(_message.Message):
    __slots__ = ("metadata",)
    METADATA_FIELD_NUMBER: _ClassVar[int]
    metadata: ResourceMetadata
    def __init__(self, metadata: _Optional[_Union[ResourceMetadata, _Mapping]] = ...) -> None: ...

class ObjectRef(_message.Message):
    __slots__ = ("atespace", "name")
    ATESPACE_FIELD_NUMBER: _ClassVar[int]
    NAME_FIELD_NUMBER: _ClassVar[int]
    atespace: str
    name: str
    def __init__(self, atespace: _Optional[str] = ..., name: _Optional[str] = ...) -> None: ...

class CreateAtespaceRequest(_message.Message):
    __slots__ = ("atespace",)
    ATESPACE_FIELD_NUMBER: _ClassVar[int]
    atespace: Atespace
    def __init__(self, atespace: _Optional[_Union[Atespace, _Mapping]] = ...) -> None: ...

class GetAtespaceRequest(_message.Message):
    __slots__ = ("atespace",)
    ATESPACE_FIELD_NUMBER: _ClassVar[int]
    atespace: ObjectRef
    def __init__(self, atespace: _Optional[_Union[ObjectRef, _Mapping]] = ...) -> None: ...

class ListAtespacesRequest(_message.Message):
    __slots__ = ()
    def __init__(self) -> None: ...

class ListAtespacesResponse(_message.Message):
    __slots__ = ("atespaces",)
    ATESPACES_FIELD_NUMBER: _ClassVar[int]
    atespaces: _containers.RepeatedCompositeFieldContainer[Atespace]
    def __init__(self, atespaces: _Optional[_Iterable[_Union[Atespace, _Mapping]]] = ...) -> None: ...

class DeleteAtespaceRequest(_message.Message):
    __slots__ = ("atespace",)
    ATESPACE_FIELD_NUMBER: _ClassVar[int]
    atespace: ObjectRef
    def __init__(self, atespace: _Optional[_Union[ObjectRef, _Mapping]] = ...) -> None: ...

class GetActorRequest(_message.Message):
    __slots__ = ("actor",)
    ACTOR_FIELD_NUMBER: _ClassVar[int]
    actor: ObjectRef
    def __init__(self, actor: _Optional[_Union[ObjectRef, _Mapping]] = ...) -> None: ...

class CreateActorRequest(_message.Message):
    __slots__ = ("actor",)
    ACTOR_FIELD_NUMBER: _ClassVar[int]
    actor: Actor
    def __init__(self, actor: _Optional[_Union[Actor, _Mapping]] = ...) -> None: ...

class UpdateActorRequest(_message.Message):
    __slots__ = ("actor", "worker_selector")
    ACTOR_FIELD_NUMBER: _ClassVar[int]
    WORKER_SELECTOR_FIELD_NUMBER: _ClassVar[int]
    actor: ObjectRef
    worker_selector: Selector
    def __init__(self, actor: _Optional[_Union[ObjectRef, _Mapping]] = ..., worker_selector: _Optional[_Union[Selector, _Mapping]] = ...) -> None: ...

class UpdateActorResponse(_message.Message):
    __slots__ = ("actor",)
    ACTOR_FIELD_NUMBER: _ClassVar[int]
    actor: Actor
    def __init__(self, actor: _Optional[_Union[Actor, _Mapping]] = ...) -> None: ...

class SuspendActorRequest(_message.Message):
    __slots__ = ("actor",)
    ACTOR_FIELD_NUMBER: _ClassVar[int]
    actor: ObjectRef
    def __init__(self, actor: _Optional[_Union[ObjectRef, _Mapping]] = ...) -> None: ...

class SuspendActorResponse(_message.Message):
    __slots__ = ("actor",)
    ACTOR_FIELD_NUMBER: _ClassVar[int]
    actor: Actor
    def __init__(self, actor: _Optional[_Union[Actor, _Mapping]] = ...) -> None: ...

class PauseActorRequest(_message.Message):
    __slots__ = ("actor",)
    ACTOR_FIELD_NUMBER: _ClassVar[int]
    actor: ObjectRef
    def __init__(self, actor: _Optional[_Union[ObjectRef, _Mapping]] = ...) -> None: ...

class PauseActorResponse(_message.Message):
    __slots__ = ("actor",)
    ACTOR_FIELD_NUMBER: _ClassVar[int]
    actor: Actor
    def __init__(self, actor: _Optional[_Union[Actor, _Mapping]] = ...) -> None: ...

class ResumeActorRequest(_message.Message):
    __slots__ = ("actor", "boot")
    ACTOR_FIELD_NUMBER: _ClassVar[int]
    BOOT_FIELD_NUMBER: _ClassVar[int]
    actor: ObjectRef
    boot: bool
    def __init__(self, actor: _Optional[_Union[ObjectRef, _Mapping]] = ..., boot: _Optional[bool] = ...) -> None: ...

class ResumeActorResponse(_message.Message):
    __slots__ = ("actor",)
    ACTOR_FIELD_NUMBER: _ClassVar[int]
    actor: Actor
    def __init__(self, actor: _Optional[_Union[Actor, _Mapping]] = ...) -> None: ...

class DeleteActorRequest(_message.Message):
    __slots__ = ("actor",)
    ACTOR_FIELD_NUMBER: _ClassVar[int]
    actor: ObjectRef
    def __init__(self, actor: _Optional[_Union[ObjectRef, _Mapping]] = ...) -> None: ...

class ListWorkersRequest(_message.Message):
    __slots__ = ()
    def __init__(self) -> None: ...

class ListWorkersResponse(_message.Message):
    __slots__ = ("workers",)
    WORKERS_FIELD_NUMBER: _ClassVar[int]
    workers: _containers.RepeatedCompositeFieldContainer[Worker]
    def __init__(self, workers: _Optional[_Iterable[_Union[Worker, _Mapping]]] = ...) -> None: ...

class ListActorsRequest(_message.Message):
    __slots__ = ("page_size", "page_token", "atespace")
    PAGE_SIZE_FIELD_NUMBER: _ClassVar[int]
    PAGE_TOKEN_FIELD_NUMBER: _ClassVar[int]
    ATESPACE_FIELD_NUMBER: _ClassVar[int]
    page_size: int
    page_token: str
    atespace: str
    def __init__(self, page_size: _Optional[int] = ..., page_token: _Optional[str] = ..., atespace: _Optional[str] = ...) -> None: ...

class ListActorsResponse(_message.Message):
    __slots__ = ("actors", "next_page_token")
    ACTORS_FIELD_NUMBER: _ClassVar[int]
    NEXT_PAGE_TOKEN_FIELD_NUMBER: _ClassVar[int]
    actors: _containers.RepeatedCompositeFieldContainer[Actor]
    next_page_token: str
    def __init__(self, actors: _Optional[_Iterable[_Union[Actor, _Mapping]]] = ..., next_page_token: _Optional[str] = ...) -> None: ...

class Worker(_message.Message):
    __slots__ = ("worker_namespace", "worker_pool", "worker_pod", "assignment", "ip", "version", "worker_pod_uid", "node_name", "sandbox_class", "labels")
    class LabelsEntry(_message.Message):
        __slots__ = ("key", "value")
        KEY_FIELD_NUMBER: _ClassVar[int]
        VALUE_FIELD_NUMBER: _ClassVar[int]
        key: str
        value: str
        def __init__(self, key: _Optional[str] = ..., value: _Optional[str] = ...) -> None: ...
    WORKER_NAMESPACE_FIELD_NUMBER: _ClassVar[int]
    WORKER_POOL_FIELD_NUMBER: _ClassVar[int]
    WORKER_POD_FIELD_NUMBER: _ClassVar[int]
    ASSIGNMENT_FIELD_NUMBER: _ClassVar[int]
    IP_FIELD_NUMBER: _ClassVar[int]
    VERSION_FIELD_NUMBER: _ClassVar[int]
    WORKER_POD_UID_FIELD_NUMBER: _ClassVar[int]
    NODE_NAME_FIELD_NUMBER: _ClassVar[int]
    SANDBOX_CLASS_FIELD_NUMBER: _ClassVar[int]
    LABELS_FIELD_NUMBER: _ClassVar[int]
    worker_namespace: str
    worker_pool: str
    worker_pod: str
    assignment: Assignment
    ip: str
    version: int
    worker_pod_uid: str
    node_name: str
    sandbox_class: str
    labels: _containers.ScalarMap[str, str]
    def __init__(self, worker_namespace: _Optional[str] = ..., worker_pool: _Optional[str] = ..., worker_pod: _Optional[str] = ..., assignment: _Optional[_Union[Assignment, _Mapping]] = ..., ip: _Optional[str] = ..., version: _Optional[int] = ..., worker_pod_uid: _Optional[str] = ..., node_name: _Optional[str] = ..., sandbox_class: _Optional[str] = ..., labels: _Optional[_Mapping[str, str]] = ...) -> None: ...

class Assignment(_message.Message):
    __slots__ = ("actor_template", "actor")
    ACTOR_TEMPLATE_FIELD_NUMBER: _ClassVar[int]
    ACTOR_FIELD_NUMBER: _ClassVar[int]
    actor_template: KubeNamespacedObjectRef
    actor: ObjectRef
    def __init__(self, actor_template: _Optional[_Union[KubeNamespacedObjectRef, _Mapping]] = ..., actor: _Optional[_Union[ObjectRef, _Mapping]] = ...) -> None: ...

class KubeNamespacedObjectRef(_message.Message):
    __slots__ = ("namespace", "name")
    NAMESPACE_FIELD_NUMBER: _ClassVar[int]
    NAME_FIELD_NUMBER: _ClassVar[int]
    namespace: str
    name: str
    def __init__(self, namespace: _Optional[str] = ..., name: _Optional[str] = ...) -> None: ...

class DebugClearRequest(_message.Message):
    __slots__ = ()
    def __init__(self) -> None: ...

class DebugClearResponse(_message.Message):
    __slots__ = ()
    def __init__(self) -> None: ...

class MintJWTRequest(_message.Message):
    __slots__ = ("audience", "app_id", "user_id", "session_id")
    AUDIENCE_FIELD_NUMBER: _ClassVar[int]
    APP_ID_FIELD_NUMBER: _ClassVar[int]
    USER_ID_FIELD_NUMBER: _ClassVar[int]
    SESSION_ID_FIELD_NUMBER: _ClassVar[int]
    audience: _containers.RepeatedScalarFieldContainer[str]
    app_id: str
    user_id: str
    session_id: str
    def __init__(self, audience: _Optional[_Iterable[str]] = ..., app_id: _Optional[str] = ..., user_id: _Optional[str] = ..., session_id: _Optional[str] = ...) -> None: ...

class MintJWTResponse(_message.Message):
    __slots__ = ("session_jwt",)
    SESSION_JWT_FIELD_NUMBER: _ClassVar[int]
    session_jwt: str
    def __init__(self, session_jwt: _Optional[str] = ...) -> None: ...

class MintCertRequest(_message.Message):
    __slots__ = ("app_id", "user_id", "session_id", "certificate_signing_request")
    APP_ID_FIELD_NUMBER: _ClassVar[int]
    USER_ID_FIELD_NUMBER: _ClassVar[int]
    SESSION_ID_FIELD_NUMBER: _ClassVar[int]
    CERTIFICATE_SIGNING_REQUEST_FIELD_NUMBER: _ClassVar[int]
    app_id: str
    user_id: str
    session_id: str
    certificate_signing_request: bytes
    def __init__(self, app_id: _Optional[str] = ..., user_id: _Optional[str] = ..., session_id: _Optional[str] = ..., certificate_signing_request: _Optional[bytes] = ...) -> None: ...

class MintCertResponse(_message.Message):
    __slots__ = ("session_certificates",)
    SESSION_CERTIFICATES_FIELD_NUMBER: _ClassVar[int]
    session_certificates: _containers.RepeatedScalarFieldContainer[bytes]
    def __init__(self, session_certificates: _Optional[_Iterable[bytes]] = ...) -> None: ...
