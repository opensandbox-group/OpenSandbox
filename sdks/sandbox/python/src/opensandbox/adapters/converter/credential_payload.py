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
"""Shared Credential Vault payload serialization for both transports.

The egress sidecar adapter and the lifecycle (server-routed) adapter send
identical vault JSON, so the request shapes are built once here and reused —
keeping the two profiles wire-compatible by construction.
"""

from opensandbox.models.sandboxes import (
    Credential,
    CredentialBinding,
    CredentialBindingMutationSet,
    CredentialMutationSet,
)


def dump_credentials(items: list[Credential | dict[str, object]]) -> list[dict[str, object]]:
    return [
        Credential.model_validate(item).model_dump(by_alias=True, exclude_none=True)
        for item in items
    ]


def dump_bindings(
    items: list[CredentialBinding | dict[str, object]],
) -> list[dict[str, object]]:
    return [
        CredentialBinding.model_validate(item).model_dump(
            by_alias=True, exclude_none=True
        )
        for item in items
    ]


def dump_credential_mutation_set(
    mutations: CredentialMutationSet | dict[str, object] | None,
) -> dict[str, object] | None:
    if mutations is None:
        return None
    parsed = CredentialMutationSet.model_validate(mutations)
    out: dict[str, object] = {}
    if parsed.add is not None:
        out["add"] = dump_credentials(parsed.add)
    if parsed.replace is not None:
        out["replace"] = dump_credentials(parsed.replace)
    if parsed.delete is not None:
        out["delete"] = list(parsed.delete)
    return out


def dump_binding_mutation_set(
    mutations: CredentialBindingMutationSet | dict[str, object] | None,
) -> dict[str, object] | None:
    if mutations is None:
        return None
    parsed = CredentialBindingMutationSet.model_validate(mutations)
    out: dict[str, object] = {}
    if parsed.add is not None:
        out["add"] = dump_bindings(parsed.add)
    if parsed.replace is not None:
        out["replace"] = dump_bindings(parsed.replace)
    if parsed.delete is not None:
        out["delete"] = list(parsed.delete)
    return out
