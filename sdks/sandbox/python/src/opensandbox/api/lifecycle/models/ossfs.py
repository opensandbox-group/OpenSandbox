#
# Copyright 2026 Alibaba Group Holding Ltd.
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
from typing import Any, TypeVar, cast

from attrs import define as _attrs_define

from ..models.ossfs_version import OSSFSVersion
from ..types import UNSET, Unset

T = TypeVar("T", bound="OSSFS")


@_attrs_define
class OSSFS:
    """Alibaba Cloud OSS mount backend via ossfs.

    The runtime mounts a host-side OSS path under `storage.ossfs_mount_root`
    and bind-mounts the resolved path into the sandbox container.
    Prefix selection is expressed via `Volume.subPath`.
    In Docker runtime, OSSFS backend requires OpenSandbox Server to run on a Linux host with FUSE support.

        Attributes:
            bucket (str): OSS bucket name.
            endpoint (str): OSS endpoint (e.g., `oss-cn-hangzhou.aliyuncs.com`).
            access_key_id (str): OSS access key ID for inline credentials mode.
            access_key_secret (str): OSS access key secret for inline credentials mode.
            version (OSSFSVersion | Unset): ossfs major version used by runtime mount integration. Default:
                OSSFSVersion.VALUE_1.
            options (list[str] | Unset): Additional ossfs mount options.
                Runtime encodes options by `version`:
                - `1.0`: mounts with `ossfs ... -o <option>`
                - `2.0`: mounts with `ossfs2 mount ... -c <config-file>` and encodes options as `--<option>` lines in the config
                file
                Option values must be provided as raw payloads without leading `-`.
    """

    bucket: str
    endpoint: str
    access_key_id: str
    access_key_secret: str
    version: OSSFSVersion | Unset = OSSFSVersion.VALUE_1
    options: list[str] | Unset = UNSET

    def to_dict(self) -> dict[str, Any]:
        bucket = self.bucket

        endpoint = self.endpoint

        access_key_id = self.access_key_id

        access_key_secret = self.access_key_secret

        version: str | Unset = UNSET
        if not isinstance(self.version, Unset):
            version = self.version.value

        options: list[str] | Unset = UNSET
        if not isinstance(self.options, Unset):
            options = self.options

        field_dict: dict[str, Any] = {}

        field_dict.update(
            {
                "bucket": bucket,
                "endpoint": endpoint,
                "accessKeyId": access_key_id,
                "accessKeySecret": access_key_secret,
            }
        )
        if version is not UNSET:
            field_dict["version"] = version
        if options is not UNSET:
            field_dict["options"] = options

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        bucket = d.pop("bucket")

        endpoint = d.pop("endpoint")

        access_key_id = d.pop("accessKeyId")

        access_key_secret = d.pop("accessKeySecret")

        _version = d.pop("version", UNSET)
        version: OSSFSVersion | Unset
        if isinstance(_version, Unset):
            version = UNSET
        else:
            version = OSSFSVersion(_version)

        options = cast(list[str], d.pop("options", UNSET))

        ossfs = cls(
            bucket=bucket,
            endpoint=endpoint,
            access_key_id=access_key_id,
            access_key_secret=access_key_secret,
            version=version,
            options=options,
        )

        return ossfs
