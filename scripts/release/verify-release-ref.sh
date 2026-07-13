#!/usr/bin/env bash

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

set -euo pipefail

remote="${RELEASE_REMOTE:-origin}"
default_branch="${RELEASE_DEFAULT_BRANCH:-main}"
release_ref="${RELEASE_REF:-${GITHUB_SHA:-HEAD}}"
remote_ref="refs/remotes/${remote}/${default_branch}"

git fetch --force --no-tags "$remote" \
  "+refs/heads/${default_branch}:${remote_ref}"

release_commit="$(git rev-parse --verify "${release_ref}^{commit}")"
default_branch_commit="$(git rev-parse --verify "${remote_ref}^{commit}")"

if ! git merge-base --is-ancestor "$release_commit" "$default_branch_commit"; then
  echo "::error::Release commit ${release_commit} is not reachable from ${remote}/${default_branch}."
  exit 1
fi

echo "Verified release commit ${release_commit} is reachable from ${remote}/${default_branch}."

if [[ -n "${GITHUB_STEP_SUMMARY:-}" ]]; then
  {
    echo "## Release source verified"
    echo
    echo "Commit \`${release_commit}\` is reachable from \`${remote}/${default_branch}\`."
  } >>"$GITHUB_STEP_SUMMARY"
fi
