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

from importlib.metadata import PackageNotFoundError
from importlib.metadata import version as _pkg_version

from opensandbox_mcp.server import create_server

try:
    __version__ = _pkg_version("opensandbox-mcp")
except PackageNotFoundError:  # pragma: no cover
    __version__ = "0.0.0"

__all__ = ["create_server"]
