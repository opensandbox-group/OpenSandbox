#
# Copyright 2026 The OpenSandbox Authors
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
#

from __future__ import annotations

from collections.abc import Mapping
from typing import TYPE_CHECKING, Any, TypeVar

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..models.server_stream_event_type import ServerStreamEventType
from ..types import UNSET, Unset

if TYPE_CHECKING:
    from ..models.server_stream_event_error import ServerStreamEventError
    from ..models.server_stream_event_results import ServerStreamEventResults


T = TypeVar("T", bound="ServerStreamEvent")


@_attrs_define
class ServerStreamEvent:
    """Server-sent event for streaming execution output

    Attributes:
        type_ (ServerStreamEventType | Unset): Event type for client-side handling Example: stdout.
        text (str | Unset): Textual data for status, init, and stream events Example: Hello, World!
            .
        execution_count (int | Unset): Cell execution number in the session Example: 1.
        exit_code (int | Unset): Successful foreground POST /command process exit status (0), present only on its
            execution_complete event after the process has exited. Omitted for background startup acknowledgements, code
            execution, and other events. Absence does not imply success; older servers omit this field. Non-zero command
            exits use error.exit_code instead.
        execution_time (int | Unset): Execution duration in milliseconds Example: 150.
        timestamp (int | Unset): When the event was generated (Unix milliseconds) Example: 1700000000000.
        results (ServerStreamEventResults | Unset): Execution output in various MIME types (e.g., "text/plain",
            "text/html") Example: {'text/plain': '4'}.
        error (ServerStreamEventError | Unset): Execution error details if an error occurred
    """

    type_: ServerStreamEventType | Unset = UNSET
    text: str | Unset = UNSET
    execution_count: int | Unset = UNSET
    exit_code: int | Unset = UNSET
    execution_time: int | Unset = UNSET
    timestamp: int | Unset = UNSET
    results: ServerStreamEventResults | Unset = UNSET
    error: ServerStreamEventError | Unset = UNSET
    additional_properties: dict[str, Any] = _attrs_field(init=False, factory=dict)

    def to_dict(self) -> dict[str, Any]:
        type_: str | Unset = UNSET
        if not isinstance(self.type_, Unset):
            type_ = self.type_.value

        text = self.text

        execution_count = self.execution_count

        exit_code = self.exit_code

        execution_time = self.execution_time

        timestamp = self.timestamp

        results: dict[str, Any] | Unset = UNSET
        if not isinstance(self.results, Unset):
            results = self.results.to_dict()

        error: dict[str, Any] | Unset = UNSET
        if not isinstance(self.error, Unset):
            error = self.error.to_dict()

        field_dict: dict[str, Any] = {}
        field_dict.update(self.additional_properties)
        field_dict.update({})
        if type_ is not UNSET:
            field_dict["type"] = type_
        if text is not UNSET:
            field_dict["text"] = text
        if execution_count is not UNSET:
            field_dict["execution_count"] = execution_count
        if exit_code is not UNSET:
            field_dict["exit_code"] = exit_code
        if execution_time is not UNSET:
            field_dict["execution_time"] = execution_time
        if timestamp is not UNSET:
            field_dict["timestamp"] = timestamp
        if results is not UNSET:
            field_dict["results"] = results
        if error is not UNSET:
            field_dict["error"] = error

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        from ..models.server_stream_event_error import ServerStreamEventError
        from ..models.server_stream_event_results import ServerStreamEventResults

        d = dict(src_dict)
        _type_ = d.pop("type", UNSET)
        type_: ServerStreamEventType | Unset
        if isinstance(_type_, Unset):
            type_ = UNSET
        else:
            type_ = ServerStreamEventType(_type_)

        text = d.pop("text", UNSET)

        execution_count = d.pop("execution_count", UNSET)

        exit_code = d.pop("exit_code", UNSET)

        execution_time = d.pop("execution_time", UNSET)

        timestamp = d.pop("timestamp", UNSET)

        _results = d.pop("results", UNSET)
        results: ServerStreamEventResults | Unset
        if isinstance(_results, Unset):
            results = UNSET
        else:
            results = ServerStreamEventResults.from_dict(_results)

        _error = d.pop("error", UNSET)
        error: ServerStreamEventError | Unset
        if isinstance(_error, Unset):
            error = UNSET
        else:
            error = ServerStreamEventError.from_dict(_error)

        server_stream_event = cls(
            type_=type_,
            text=text,
            execution_count=execution_count,
            exit_code=exit_code,
            execution_time=execution_time,
            timestamp=timestamp,
            results=results,
            error=error,
        )

        server_stream_event.additional_properties = d
        return server_stream_event

    @property
    def additional_keys(self) -> list[str]:
        return list(self.additional_properties.keys())

    def __getitem__(self, key: str) -> Any:
        return self.additional_properties[key]

    def __setitem__(self, key: str, value: Any) -> None:
        self.additional_properties[key] = value

    def __delitem__(self, key: str) -> None:
        del self.additional_properties[key]

    def __contains__(self, key: str) -> bool:
        return key in self.additional_properties
