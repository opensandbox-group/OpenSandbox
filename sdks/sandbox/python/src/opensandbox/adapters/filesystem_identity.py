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

"""Shared identity validation for synchronous and asynchronous file clients."""

from opensandbox.exceptions import InvalidArgumentException


def filesystem_identity_path(uid: int, gid: int) -> str:
    for name, value in (("uid", uid), ("gid", gid)):
        if type(value) is not int or not 0 <= value <= 4294967294:
            raise InvalidArgumentException(
                f"{name} must be an integer between 0 and 4294967294"
            )
    return f"/v1/filesystem/{uid}/{gid}"
