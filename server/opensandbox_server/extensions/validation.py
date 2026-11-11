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

from __future__ import annotations

import logging
from typing import TYPE_CHECKING, Dict, Optional

from fastapi import HTTPException, status

from opensandbox_server.constants import (
    RENEW_EXTEND_MAX_SECONDS,
    RENEW_EXTEND_MIN_SECONDS,
)
from opensandbox_server.extensions.codec import access_renew_extend_seconds_from_lifecycle
from opensandbox_server.extensions.keys import ACCESS_RENEW_EXTEND_SECONDS_KEY
from opensandbox_server.services.constants import SandboxErrorCodes

if TYPE_CHECKING:
    from opensandbox_server.api.schema import SandboxLifecycle

logger = logging.getLogger(__name__)

# Shared bounds (constants.py) re-exported under the historical names.
ACCESS_RENEW_EXTEND_SECONDS_MIN = RENEW_EXTEND_MIN_SECONDS
ACCESS_RENEW_EXTEND_SECONDS_MAX = RENEW_EXTEND_MAX_SECONDS

_LEGACY_RENEW_KEY_DEPRECATION = (
    'extensions["%s"] is deprecated: use lifecycle.renewalPolicy.extendSeconds '
    "instead. The extensions key stays supported throughout 1.x and is "
    "removed in 2.0."
)

_access_renew_deprecation_logged = False


def _warn_access_renew_deprecation() -> None:
    """Log the legacy renew-key deprecation once per process."""
    global _access_renew_deprecation_logged
    if _access_renew_deprecation_logged:
        return
    _access_renew_deprecation_logged = True
    logger.warning(_LEGACY_RENEW_KEY_DEPRECATION, ACCESS_RENEW_EXTEND_SECONDS_KEY)


# TODO(2.0): remove with the legacy request entry (#2289).
def _validate_access_renew_extend_seconds(extensions: Dict[str, str]) -> None:
    """
    Require the deprecated ``access.renew.extend.seconds`` to be an integer
    in [MIN, MAX] seconds; use logs a deprecation warning.

    Args:
        extensions: Non-empty extension map (may omit this key).

    Raises:
        HTTPException: 400 when the key is present but invalid.
    """
    key = ACCESS_RENEW_EXTEND_SECONDS_KEY
    if key not in extensions:
        return
    _warn_access_renew_deprecation()
    raw = extensions[key]
    if raw is None:
        raise HTTPException(
            status_code=status.HTTP_400_BAD_REQUEST,
            detail={
                "code": SandboxErrorCodes.INVALID_PARAMETER,
                "message": (
                    f'Invalid extensions["{key}"]: expected a string of digits between '
                    f"{ACCESS_RENEW_EXTEND_SECONDS_MIN} and {ACCESS_RENEW_EXTEND_SECONDS_MAX} "
                    "(5 minutes to 24 hours inclusive). Prefer lifecycle.renewalPolicy."
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
                    f"{ACCESS_RENEW_EXTEND_SECONDS_MAX} seconds. Prefer lifecycle.renewalPolicy."
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
                    f"between {ACCESS_RENEW_EXTEND_SECONDS_MIN} and {ACCESS_RENEW_EXTEND_SECONDS_MAX}, got {raw!r}. "
                    "Prefer lifecycle.renewalPolicy."
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
                    f"{ACCESS_RENEW_EXTEND_SECONDS_MAX} seconds (5 minutes to 24 hours inclusive), got {n}. "
                    "Prefer lifecycle.renewalPolicy."
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


def validate_renew_policy_conflict(
    lifecycle: Optional["SandboxLifecycle"],
    extensions: Optional[Dict[str, str]],
) -> None:
    """
    Reject declaring the renew policy via both ``lifecycle.renewalPolicy``
    and the deprecated extensions key with different parsed values (400);
    unparseable legacy values stay with ``validate_extensions``.

    Args:
        lifecycle: Optional lifecycle config from the create request.
        extensions: Optional opaque extension map from the create request.

    Raises:
        HTTPException: 400 when both inputs are set with different values.

    TODO(2.0): remove with the legacy request entry (#2289).
    """
    declared = access_renew_extend_seconds_from_lifecycle(lifecycle)
    if declared is None or not extensions:
        return
    raw = extensions.get(ACCESS_RENEW_EXTEND_SECONDS_KEY)
    if raw is None or not str(raw).strip():
        return
    try:
        legacy = int(str(raw).strip())
    except ValueError:
        return
    if legacy != declared:
        raise HTTPException(
            status_code=status.HTTP_400_BAD_REQUEST,
            detail={
                "code": SandboxErrorCodes.INVALID_PARAMETER,
                "message": (
                    "lifecycle.renewalPolicy.extendSeconds and the deprecated "
                    f'extensions["{ACCESS_RENEW_EXTEND_SECONDS_KEY}"] must match '
                    f"when both are set (values compared after parsing), got "
                    f"{declared} and {legacy}; prefer lifecycle.renewalPolicy."
                ),
            },
        )
