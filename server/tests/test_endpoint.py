# Copyright 2025 Alibaba Group Holding Ltd.
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

from opensandbox_server.services.helpers import normalize_external_endpoint_url


def test_normalize_external_endpoint_url_defaults_to_https() -> None:
    assert (
        normalize_external_endpoint_url("oss-cn-hangzhou.aliyuncs.com")
        == "https://oss-cn-hangzhou.aliyuncs.com"
    )


def test_normalize_external_endpoint_url_keeps_existing_scheme() -> None:
    assert (
        normalize_external_endpoint_url("http://oss-cn-hangzhou.aliyuncs.com")
        == "http://oss-cn-hangzhou.aliyuncs.com"
    )
