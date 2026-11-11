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

import pytest
from fastapi import HTTPException

from opensandbox_server.api.schema import SandboxLifecycle
from opensandbox_server.extensions import (
    ACCESS_RENEW_EXTEND_SECONDS_KEY,
    ACCESS_RENEW_EXTEND_SECONDS_METADATA_KEY,
    access_renew_extend_seconds_from_lifecycle,
    apply_access_renew_extend_seconds_to_mapping,
    apply_extensions_to_mapping,
    extract_extensions_from_mapping,
    validate_extensions,
    validate_renew_policy_conflict,
)


def _lifecycle(extend_seconds: int | None) -> SandboxLifecycle:
    if extend_seconds is None:
        return SandboxLifecycle()
    return SandboxLifecycle(renewalPolicy={"extendSeconds": extend_seconds})


class TestAccessRenewExtendSecondsFromLifecycle:
    """lifecycle.renewalPolicy (OSEP-0009) resolution."""

    def test_none_lifecycle(self):
        assert access_renew_extend_seconds_from_lifecycle(None) is None

    def test_lifecycle_without_policy(self):
        assert access_renew_extend_seconds_from_lifecycle(_lifecycle(None)) is None

    def test_lifecycle_with_policy(self):
        assert access_renew_extend_seconds_from_lifecycle(_lifecycle(3600)) == 3600


class TestApplyAccessRenewExtendSecondsWithLifecycle:
    def test_lifecycle_value_written(self):
        m: dict[str, str] = {}
        apply_access_renew_extend_seconds_to_mapping(m, None, lifecycle=_lifecycle(3600))
        assert m == {ACCESS_RENEW_EXTEND_SECONDS_METADATA_KEY: "3600"}

    def test_lifecycle_preferred_over_legacy_extensions(self):
        m: dict[str, str] = {}
        apply_access_renew_extend_seconds_to_mapping(
            m,
            {ACCESS_RENEW_EXTEND_SECONDS_KEY: "3600"},
            lifecycle=_lifecycle(3600),
        )
        assert m == {ACCESS_RENEW_EXTEND_SECONDS_METADATA_KEY: "3600"}

    def test_legacy_extensions_fallback_without_lifecycle_policy(self):
        m: dict[str, str] = {}
        apply_access_renew_extend_seconds_to_mapping(
            m, {ACCESS_RENEW_EXTEND_SECONDS_KEY: "1800"}, lifecycle=_lifecycle(None)
        )
        assert m == {ACCESS_RENEW_EXTEND_SECONDS_METADATA_KEY: "1800"}

    def test_noop_when_both_absent(self):
        m: dict[str, str] = {"x": "1"}
        apply_access_renew_extend_seconds_to_mapping(m, None, lifecycle=_lifecycle(None))
        assert m == {"x": "1"}


class TestRenewPolicyConflict:
    """Both renewal-policy inputs must agree on the parsed value."""

    def test_no_lifecycle_ok(self):
        assert (
            validate_renew_policy_conflict(
                None, {ACCESS_RENEW_EXTEND_SECONDS_KEY: "3600"}
            )
            is None
        )

    def test_lifecycle_without_policy_ok(self):
        assert (
            validate_renew_policy_conflict(
                _lifecycle(None), {ACCESS_RENEW_EXTEND_SECONDS_KEY: "3600"}
            )
            is None
        )

    def test_extensions_only_ok(self):
        assert validate_renew_policy_conflict(_lifecycle(3600), None) is None
        assert validate_renew_policy_conflict(_lifecycle(3600), {"other": "x"}) is None

    def test_equal_values_ok(self):
        assert (
            validate_renew_policy_conflict(
                _lifecycle(3600), {ACCESS_RENEW_EXTEND_SECONDS_KEY: "3600"}
            )
            is None
        )

    def test_equal_values_whitespace_ok(self):
        assert (
            validate_renew_policy_conflict(
                _lifecycle(3600), {ACCESS_RENEW_EXTEND_SECONDS_KEY: "  3600  "}
            )
            is None
        )

    def test_conflicting_values_rejected(self):
        with pytest.raises(HTTPException) as exc:
            validate_renew_policy_conflict(
                _lifecycle(3600), {ACCESS_RENEW_EXTEND_SECONDS_KEY: "1800"}
            )
        assert exc.value.status_code == 400
        assert "renewalPolicy" in exc.value.detail["message"]

    def test_non_integer_legacy_value_defers_to_validate_extensions(self):
        # Format errors stay with validate_extensions, not the conflict check.
        assert (
            validate_renew_policy_conflict(
                _lifecycle(3600), {ACCESS_RENEW_EXTEND_SECONDS_KEY: "abc"}
            )
            is None
        )


class TestLegacyRenewKeyValidationSmoke:
    """Smoke coverage for the deprecated key (removed in 2.0)."""

    def test_valid_and_invalid_values(self):
        assert validate_extensions({ACCESS_RENEW_EXTEND_SECONDS_KEY: "  1800  "}) is None
        with pytest.raises(HTTPException) as exc:
            validate_extensions({ACCESS_RENEW_EXTEND_SECONDS_KEY: "299"})
        assert exc.value.status_code == 400

    def test_deprecation_warned_once_per_process(self, monkeypatch):
        from opensandbox_server.extensions import validation as ext_validation

        messages: list[str] = []
        monkeypatch.setattr(
            ext_validation.logger,
            "warning",
            lambda msg, *args: messages.append(msg % args if args else msg),
        )

        ext_validation._access_renew_deprecation_logged = False
        try:
            assert validate_extensions({ACCESS_RENEW_EXTEND_SECONDS_KEY: "3600"}) is None
            assert validate_extensions({ACCESS_RENEW_EXTEND_SECONDS_KEY: "1800"}) is None
        finally:
            ext_validation._access_renew_deprecation_logged = False

        assert len(messages) == 1
        assert ACCESS_RENEW_EXTEND_SECONDS_KEY in messages[0]
        assert "lifecycle.renewalPolicy" in messages[0]


class TestExtensionsToAnnotations:
    """Extensions with opensandbox.extensions. prefix are propagated to annotations."""

    def test_single_extension_propagated(self):
        annotations: dict[str, str] = {}
        extensions = {"opensandbox.extensions.pool-ref": "my-pool"}
        apply_extensions_to_mapping(annotations, extensions)
        assert annotations == {"opensandbox.io/extensions.pool-ref": "my-pool"}

    def test_multiple_extensions_propagated(self):
        annotations: dict[str, str] = {}
        extensions = {
            "opensandbox.extensions.pool-ref": "my-pool",
            "opensandbox.extensions.custom-key": "custom-value",
        }
        apply_extensions_to_mapping(annotations, extensions)
        assert annotations == {
            "opensandbox.io/extensions.pool-ref": "my-pool",
            "opensandbox.io/extensions.custom-key": "custom-value",
        }

    def test_non_prefix_extension_not_propagated(self):
        annotations: dict[str, str] = {}
        extensions = {
            "poolRef": "my-pool",
            "other.key": "value",
        }
        apply_extensions_to_mapping(annotations, extensions)
        assert annotations == {}

    def test_mixed_extensions_propagated_only_prefix_keys(self):
        annotations: dict[str, str] = {}
        extensions = {
            "opensandbox.extensions.pool-ref": "my-pool",
            "poolRef": "ignored",
        }
        apply_extensions_to_mapping(annotations, extensions)
        assert annotations == {"opensandbox.io/extensions.pool-ref": "my-pool"}

    def test_empty_extensions_noop(self):
        annotations: dict[str, str] = {"existing": "value"}
        apply_extensions_to_mapping(annotations, None)
        assert annotations == {"existing": "value"}

    def test_empty_extensions_dict_noop(self):
        annotations: dict[str, str] = {"existing": "value"}
        apply_extensions_to_mapping(annotations, {})
        assert annotations == {"existing": "value"}

    def test_preserves_existing_annotations(self):
        annotations: dict[str, str] = {"existing": "value"}
        extensions = {"opensandbox.extensions.new-key": "new-value"}
        apply_extensions_to_mapping(annotations, extensions)
        assert annotations == {
            "existing": "value",
            "opensandbox.io/extensions.new-key": "new-value",
        }


class TestExtensionsFromAnnotations:
    def test_restores_prefixed_annotations(self):
        annotations = {
            "opensandbox.io/extensions.custom-key": "custom-value",
            "opensandbox.io/extensions.localized": "中文数据",
        }

        assert extract_extensions_from_mapping(annotations) == {
            "opensandbox.extensions.custom-key": "custom-value",
            "opensandbox.extensions.localized": "中文数据",
        }

    def test_ignores_non_extension_annotations(self):
        annotations = {
            "opensandbox.io/access-renew-extend-seconds": "1800",
            "other": "value",
        }

        assert extract_extensions_from_mapping(annotations) is None

    def test_skips_empty_values(self):
        annotations = {
            "opensandbox.io/extensions.blanked": "",
            "opensandbox.io/extensions.custom-key": "custom-value",
        }

        assert extract_extensions_from_mapping(annotations) == {
            "opensandbox.extensions.custom-key": "custom-value",
        }
        assert extract_extensions_from_mapping({"opensandbox.io/extensions.blanked": ""}) is None

    def test_empty_annotations_return_none(self):
        assert extract_extensions_from_mapping({}) is None
        assert extract_extensions_from_mapping(None) is None
