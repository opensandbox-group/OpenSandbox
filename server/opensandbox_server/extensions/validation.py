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

from __future__ import annotations

from typing import Dict, Optional

from fastapi import HTTPException, status

from opensandbox_server.extensions.keys import ACCESS_RENEW_EXTEND_SECONDS_KEY
from opensandbox_server.services.constants import SandboxErrorCodes

ACCESS_RENEW_EXTEND_SECONDS_MIN = 300  # 5 minutes
ACCESS_RENEW_EXTEND_SECONDS_MAX = 86400  # 24 hours


def _validate_access_renew_extend_seconds(extensions: Dict[str, str]) -> None:
    """
    If ``access.renew.extend.seconds`` is set, require a base-10 integer in [MIN, MAX] seconds.

    Args:
        extensions: Non-empty extension map (may omit this key).

    Raises:
        HTTPException: 400 when the key is present but invalid.
    """
    key = ACCESS_RENEW_EXTEND_SECONDS_KEY
    if key not in extensions:
        return
    raw = extensions[key]
    if raw is None:
        raise HTTPException(
            status_code=status.HTTP_400_BAD_REQUEST,
            detail={
                "code": SandboxErrorCodes.INVALID_PARAMETER,
                "message": (
                    f'Invalid extensions["{key}"]: expected a string of digits between '
                    f"{ACCESS_RENEW_EXTEND_SECONDS_MIN} and {ACCESS_RENEW_EXTEND_SECONDS_MAX} "
                    "(5 minutes to 24 hours inclusive)."
                ),
            },
        )
    s = str(raw).strip()
    if not s:
        raise HTTPException(
            status_code=status.HTTP_400_BAD_REQUEST,
            detail={
                "code": SandboxErrorCodes.INVALID_PARAMETER,
                "message": (
                    f'Invalid extensions["{key}"]: empty value; omit the key to disable renew-on-access, '
                    f"or use an integer between {ACCESS_RENEW_EXTEND_SECONDS_MIN} and "
                    f"{ACCESS_RENEW_EXTEND_SECONDS_MAX} seconds."
                ),
            },
        )
    try:
        n = int(s)
    except ValueError:
        raise HTTPException(
            status_code=status.HTTP_400_BAD_REQUEST,
            detail={
                "code": SandboxErrorCodes.INVALID_PARAMETER,
                "message": (
                    f'Invalid extensions["{key}"]: must be a base-10 integer string '
                    f"between {ACCESS_RENEW_EXTEND_SECONDS_MIN} and {ACCESS_RENEW_EXTEND_SECONDS_MAX}, got {raw!r}."
                ),
            },
        ) from None
    if n < ACCESS_RENEW_EXTEND_SECONDS_MIN or n > ACCESS_RENEW_EXTEND_SECONDS_MAX:
        raise HTTPException(
            status_code=status.HTTP_400_BAD_REQUEST,
            detail={
                "code": SandboxErrorCodes.INVALID_PARAMETER,
                "message": (
                    f'Invalid extensions["{key}"]: must be between {ACCESS_RENEW_EXTEND_SECONDS_MIN} and '
                    f"{ACCESS_RENEW_EXTEND_SECONDS_MAX} seconds (5 minutes to 24 hours inclusive), got {n}."
                ),
            },
        )


def validate_extensions(extensions: Optional[Dict[str, str]]) -> None:
    """
    Validate well-known keys in ``extensions`` for sandbox creation.

    Args:
        extensions: Optional opaque extension map from the create request.

    Raises:
        HTTPException: 400 when a known key is present but invalid.
    """
    if not extensions:
        return

    _validate_access_renew_extend_seconds(extensions)
