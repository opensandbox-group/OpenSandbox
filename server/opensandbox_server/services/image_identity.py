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

"""Registry image identity observed from runtime metadata, never request guesses."""

import re
from typing import Any


_REGISTRY_DIGEST = re.compile(r"([^\s@]+)@(sha256:[0-9a-f]{64})")


def registry_image_digest(reference: Any) -> str | None:
    """Accept registry-qualified identities, not runtime image/config IDs."""
    if not isinstance(reference, str):
        return None
    if reference.startswith("docker-pullable://"):
        reference = reference.removeprefix("docker-pullable://")
    if "://" in reference:
        return None
    match = _REGISTRY_DIGEST.fullmatch(reference)
    return match.group(2) if match else None


def _repository(reference: str) -> str:
    repository = reference.split("@", 1)[0]
    if ":" in repository.rsplit("/", 1)[-1]:
        repository = repository.rsplit(":", 1)[0]
    first = repository.split("/", 1)[0]
    if "/" not in repository or ("." not in first and ":" not in first and first != "localhost"):
        repository = "docker.io/" + repository
    repository = re.sub(r"^(index\.docker\.io|registry-1\.docker\.io)/", "docker.io/", repository)
    if repository.startswith("docker.io/") and repository.count("/") == 1:
        repository = repository.replace("docker.io/", "docker.io/library/", 1)
    return repository


def docker_image_digest(container: Any) -> str | None:
    """Use only RepoDigests from the image attached to this container."""
    try:
        attrs = container.image.attrs
        if not isinstance(attrs, dict):
            return None
        references = attrs.get("RepoDigests")
        if not isinstance(references, list):
            return None
        identities = {ref: registry_image_digest(ref) for ref in references if isinstance(ref, str)}
        identities = {ref: digest for ref, digest in identities.items() if digest is not None}
        # Containerd-backed Docker also synthesizes RepoDigests for local builds.
        # Require matching pull provenance when the daemon exposes it.
        if "Identity" in attrs or "Descriptor" in attrs:
            identity = attrs.get("Identity")
            pulls = identity.get("Pull") if isinstance(identity, dict) else None
            if not isinstance(pulls, list):
                return None
            repositories = {
                _repository(pull["Repository"])
                for pull in pulls
                if isinstance(pull, dict)
                and isinstance(pull.get("Repository"), str)
                and pull["Repository"]
            }
            identities = {
                ref: digest
                for ref, digest in identities.items()
                if _repository(ref) in repositories
            }
        digests = set(identities.values())
        if len(digests) == 1:
            return next(iter(digests))
        config = container.attrs.get("Config") or {}
        requested = config.get("Image")
        if not isinstance(requested, str) or not requested:
            return None
        matching = {
            digest
            for ref, digest in identities.items()
            if _repository(ref) == _repository(requested)
        }
        return next(iter(matching)) if len(matching) == 1 else None
    except Exception:
        # Optional runtime inspection must not make a lifecycle response fail.
        return None
