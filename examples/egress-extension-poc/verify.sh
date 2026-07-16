#!/usr/bin/env bash

set -euo pipefail

base_url="${POC_BASE_URL:-http://127.0.0.1:28080}"
auth_token="${POC_AUTH_TOKEN:-poc-token}"
tmp_body="$(mktemp)"
trap 'rm -f "$tmp_body"' EXIT

request() {
  curl --fail-with-body --silent --show-error \
    -H "OPENSANDBOX-EGRESS-AUTH: ${auth_token}" \
    -H "Content-Type: application/json" \
    "$@"
}

echo "Checking authentication"
unauthorized_status="$(curl --silent --show-error -o "${tmp_body}" -w '%{http_code}' \
  "${base_url}/capabilities")"
[[ "${unauthorized_status}" == "401" ]]

echo "Checking extension capabilities at ${base_url}"
capabilities="$(request "${base_url}/capabilities")"
jq -e '
  .protocolVersion == "extensions.opensandbox.io/v1alpha1" and
  any(.resources[];
    .apiVersion == "gateway.networking.k8s.io/v1" and
    .kind == "HTTPRoute" and
    .available == true and
    (.operations | index("replace")) != null and
    (.features | index("extensions.opensandbox.io/StorageOnlyPOC")) != null)
' <<<"${capabilities}" >/dev/null

current="$(request "${base_url}/extensions")"
revision="$(jq -er '.revision' <<<"${current}")"

payload="$(jq -n --argjson revision "${revision}" '{
  expectedRevision: $revision,
  resources: [{
    apiVersion: "gateway.networking.k8s.io/v1",
    kind: "HTTPRoute",
    metadata: {
      name: "api-example",
      futureMetadata: {preserved: true}
    },
    spec: {
      hostnames: ["api.example.com"],
      rules: [{matches: [{method: "GET"}]}],
      futureField: {preserved: [1, 2, 3]}
    }
  }]
}')"

echo "Replacing extension revision ${revision}"
replaced="$(request -X PUT "${base_url}/extensions" --data "${payload}")"
new_revision="$(jq -er '.revision' <<<"${replaced}")"
jq -e --argjson expected "$((revision + 1))" '
  .revision == $expected and
  .resources[0].metadata.futureMetadata.preserved == true and
  .resources[0].spec.futureField.preserved == [1, 2, 3] and
  any(.conditions[];
    .type == "Programmed" and
    .status == "Unknown" and
    .reason == "PocStored")
' <<<"${replaced}" >/dev/null

round_trip="$(request "${base_url}/extensions")"
jq -e --argjson expected "${new_revision}" '
  .revision == $expected and
  .resources[0].metadata.futureMetadata.preserved == true and
  .resources[0].spec.futureField.preserved == [1, 2, 3]
' <<<"${round_trip}" >/dev/null

echo "Checking stale-revision rejection"
stale_status="$(curl --silent --show-error -o "${tmp_body}" -w '%{http_code}' \
  -X PUT "${base_url}/extensions" \
  -H "OPENSANDBOX-EGRESS-AUTH: ${auth_token}" \
  -H "Content-Type: application/json" \
  --data "$(jq -n --argjson revision "${revision}" '{expectedRevision: $revision, resources: []}')")"
[[ "${stale_status}" == "409" ]]
jq -e '.code == "REVISION_CONFLICT"' "${tmp_body}" >/dev/null

echo "Checking unavailable TLSRoute rejection"
unavailable_status="$(curl --silent --show-error -o "${tmp_body}" -w '%{http_code}' \
  -X PUT "${base_url}/extensions" \
  -H "OPENSANDBOX-EGRESS-AUTH: ${auth_token}" \
  -H "Content-Type: application/json" \
  --data "$(jq -n --argjson revision "${new_revision}" '{
    expectedRevision: $revision,
    resources: [{
      apiVersion: "gateway.networking.k8s.io/v1alpha2",
      kind: "TLSRoute",
      metadata: {name: "api-example-tls"},
      spec: {hostnames: ["api.example.com"]}
    }]
  }')")"
[[ "${unavailable_status}" == "412" ]]
jq -e '.code == "CAPABILITY_UNAVAILABLE"' "${tmp_body}" >/dev/null

echo "Clearing the stored extension set"
cleared="$(request -X PUT "${base_url}/extensions" \
  --data "$(jq -n --argjson revision "${new_revision}" '{expectedRevision: $revision, resources: []}')")"
jq -e --argjson expected "$((new_revision + 1))" '
  .revision == $expected and
  (.resources | length) == 0 and
  any(.conditions[];
    .type == "Programmed" and
    .status == "Unknown" and
    .reason == "PocStored")
' <<<"${cleared}" >/dev/null

echo "POC verification passed (final revision $(jq -r '.revision' <<<"${cleared}"))"
