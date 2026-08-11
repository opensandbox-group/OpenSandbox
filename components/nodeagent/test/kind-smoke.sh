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

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
cluster_name="${KIND_CLUSTER_NAME:-nodeagent-smoke}"
image="opensandbox/nodeagent:kind-smoke"
node="${cluster_name}-control-plane"
cluster_created=0
temp_files=()

cleanup() {
	if [[ "${KEEP_KIND_CLUSTER:-}" == "1" && "${cluster_created}" == "1" ]]; then
		echo "keeping Kind cluster ${cluster_name} for inspection" >&2
		echo "run: kind export kubeconfig --name ${cluster_name}" >&2
	elif [[ "${cluster_created}" == "1" ]]; then
		kind delete cluster --name "${cluster_name}" >/dev/null 2>&1 || true
	fi
	if [[ ${#temp_files[@]} -gt 0 ]]; then
		rm -f "${temp_files[@]}"
	fi
}

finish() {
	status=$?
	trap - EXIT
	if [[ ${status} -ne 0 && "${cluster_created}" == "1" ]]; then
		echo "Kind smoke test failed; collecting diagnostics" >&2
		kubectl get pods -A -o wide >&2 || true
		kubectl logs -n opensandbox-system -l app.kubernetes.io/component=node-agent --all-containers --tail=-1 >&2 || true
		docker exec "${node}" sh -c 'find /var/lib/opensandbox/nodeagent /var/lib/opensandbox/nodeagent-data -maxdepth 6 -ls 2>/dev/null' >&2 || true
	fi
	cleanup
	exit "${status}"
}
trap finish EXIT

kubeconfig="$(mktemp)"
temp_files+=("${kubeconfig}")
find_stderr="$(mktemp)"
temp_files+=("${find_stderr}")
marker_stderr="$(mktemp)"
temp_files+=("${marker_stderr}")
jq_stderr="$(mktemp)"
temp_files+=("${jq_stderr}")
pool_stderr="$(mktemp)"
temp_files+=("${pool_stderr}")
agent_uid_stderr="$(mktemp)"
temp_files+=("${agent_uid_stderr}")
logs_stderr="$(mktemp)"
temp_files+=("${logs_stderr}")
pool_family_stderr="$(mktemp)"
temp_files+=("${pool_family_stderr}")
export KUBECONFIG="${kubeconfig}"

for required_command in kind kubectl helm docker jq; do
  if ! command -v "${required_command}" >/dev/null 2>&1; then
    echo "required command is missing: ${required_command}" >&2
    exit 1
  fi
done

if ! existing_clusters="$(kind get clusters)"; then
  echo "failed to list existing Kind clusters; refusing destructive cleanup" >&2
  exit 1
fi
if grep -Fxq "${cluster_name}" <<<"${existing_clusters}"; then
  echo "Kind cluster already exists; refusing to reuse or delete it: ${cluster_name}" >&2
  echo "remove it explicitly with: kind delete cluster --name ${cluster_name}" >&2
  exit 1
fi
kind create cluster --name "${cluster_name}" --kubeconfig "${kubeconfig}" --wait 120s
cluster_created=1
DOCKER_BUILDKIT=1 docker build -f "${repo_root}/components/nodeagent/Dockerfile" -t "${image}" "${repo_root}"
kind load docker-image --name "${cluster_name}" "${image}"

helm install nodeagent "${repo_root}/kubernetes/charts/opensandbox-node-agent" \
  --namespace opensandbox-system \
  --create-namespace \
  --set-string image.repository="${image%:*}" \
  --set-string image.tag="${image##*:}" \
  --set image.pullPolicy=Never \
  --set config.clusterID=kind-test \
  --set config.partialTimeout=1s \
  --set config.endedStateRetention=1m \
  --set sink.type=file \
  --wait --timeout 180s

kubectl create namespace workloads
sed "s|NODEAGENT_SMOKE_IMAGE|${image}|g" <<'YAML' | kubectl apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: normal-sandbox
  namespace: workloads
  labels:
    opensandbox.io/id: sb-normal
spec:
  restartPolicy: Never
  containers:
    - name: sandbox
      image: NODEAGENT_SMOKE_IMAGE
      imagePullPolicy: Never
      command: ["sh", "-c", "echo before-restart; while [ ! -f /tmp/emit-during-restart ]; do sleep 1; done; echo during-agent-restart; while [ ! -f /tmp/release ]; do sleep 1; done; echo after-restart"]
---
apiVersion: v1
kind: Pod
metadata:
  name: pooled-sandbox
  namespace: workloads
  labels:
    opensandbox.io/id: sb-pool
    sandbox.opensandbox.io/pool-name: test-pool
spec:
  restartPolicy: Never
  containers:
    - name: sandbox
      image: NODEAGENT_SMOKE_IMAGE
      imagePullPolicy: Never
      command: ["sh", "-c", "echo must-not-be-collected; sleep 2"]
YAML

kubectl wait --for=jsonpath='{.status.phase}'=Succeeded pod/pooled-sandbox -n workloads --timeout=120s
pool_probe="must-not-be-collected"
pool_source_ready=0
for _ in $(seq 1 30); do
	if docker exec "${node}" sh -c 'grep -R -Fq -- "$1" /var/log/pods/workloads_pooled-sandbox_*/sandbox/' _ "${pool_probe}"; then
		pool_source_ready=1
		break
	fi
	sleep 1
done
if [[ ${pool_source_ready} -ne 1 ]]; then
	echo "kubelet did not write the Pool Pod probe string; the Pool exclusion assertion would be vacuous" >&2
	exit 1
fi

for _ in $(seq 1 90); do
  if docker exec "${node}" sh -c 'grep -R -q "before-restart" /var/lib/opensandbox/nodeagent-data 2>/dev/null'; then
    break
  fi
  sleep 1
done
docker exec "${node}" sh -c 'grep -R -q "before-restart" /var/lib/opensandbox/nodeagent-data'

old_agent_pod="$(kubectl get pod -n opensandbox-system -l app.kubernetes.io/component=node-agent -o jsonpath='{.items[0].metadata.name}')"
old_agent_uid="$(kubectl get pod "${old_agent_pod}" -n opensandbox-system -o jsonpath='{.metadata.uid}')"
kubectl patch daemonset/nodeagent-opensandbox-node-agent -n opensandbox-system --type=merge \
  -p '{"spec":{"template":{"spec":{"nodeSelector":{"nodeagent.opensandbox.io/smoke-pause":"true"}}}}}'
kubectl wait --for=delete "pod/${old_agent_pod}" -n opensandbox-system --timeout=120s
agent_pods="$(kubectl get pod -n opensandbox-system -l app.kubernetes.io/component=node-agent -o name)"
if [[ -n "${agent_pods}" ]]; then
  echo "Node Agent Pod still exists during the recovery test outage" >&2
  exit 1
fi
kubectl exec -n workloads normal-sandbox -- touch /tmp/emit-during-restart
sandbox_logs=""
for _ in $(seq 1 30); do
  if sandbox_logs="$(kubectl logs pod/normal-sandbox -n workloads 2>"${logs_stderr}")"; then
    if grep -Fxq 'during-agent-restart' <<<"${sandbox_logs}"; then
      break
    fi
  fi
  sleep 1
done
if ! grep -Fxq 'during-agent-restart' <<<"${sandbox_logs}"; then
  echo "outage log line was not observed" >&2
  if [[ -s "${logs_stderr}" ]]; then
    echo "last sandbox log query error: $(<"${logs_stderr}")" >&2
  fi
  exit 1
fi
agent_pods="$(kubectl get pod -n opensandbox-system -l app.kubernetes.io/component=node-agent -o name)"
if [[ -n "${agent_pods}" ]]; then
  echo "Node Agent Pod restarted before the outage log was written" >&2
  exit 1
fi
kubectl patch daemonset/nodeagent-opensandbox-node-agent -n opensandbox-system --type=merge \
  -p '{"spec":{"template":{"spec":{"nodeSelector":{"nodeagent.opensandbox.io/smoke-pause":null}}}}}'
new_agent_uid=""
for _ in $(seq 1 120); do
  new_agent_uid=""
  new_agent_uids=""
  if ! new_agent_uids="$(kubectl get pod -n opensandbox-system -l app.kubernetes.io/component=node-agent -o jsonpath='{.items[*].metadata.uid}' 2>"${agent_uid_stderr}")"; then
    sleep 1
    continue
  fi
  for candidate_uid in ${new_agent_uids}; do
    if [[ "${candidate_uid}" != "${old_agent_uid}" ]]; then
      new_agent_uid="${candidate_uid}"
      break
    fi
  done
  if [[ -n "${new_agent_uid}" ]]; then
    break
  fi
  sleep 1
done
if [[ -z "${new_agent_uid}" ]]; then
  echo "Node Agent Pod was not replaced" >&2
  if [[ -s "${agent_uid_stderr}" ]]; then
    echo "last Agent Pod query error: $(<"${agent_uid_stderr}")" >&2
  fi
  exit 1
fi
kubectl rollout status daemonset/nodeagent-opensandbox-node-agent -n opensandbox-system --timeout=120s
kubectl wait --for=condition=Ready pod -n opensandbox-system -l app.kubernetes.io/component=node-agent --timeout=120s

for _ in $(seq 1 90); do
  if docker exec "${node}" sh -c 'grep -R -q "during-agent-restart" /var/lib/opensandbox/nodeagent-data 2>/dev/null'; then
    break
  fi
  sleep 1
done
docker exec "${node}" sh -c 'grep -R -q "during-agent-restart" /var/lib/opensandbox/nodeagent-data'

kubectl exec -n workloads normal-sandbox -- touch /tmp/release

for _ in $(seq 1 90); do
  if docker exec "${node}" sh -c 'grep -R -q "after-restart" /var/lib/opensandbox/nodeagent-data 2>/dev/null'; then
    break
  fi
  sleep 1
done
docker exec "${node}" sh -c 'grep -R -q "after-restart" /var/lib/opensandbox/nodeagent-data'
kubectl wait --for=jsonpath='{.status.phase}'=Succeeded pod/normal-sandbox -n workloads --timeout=120s
marker=""
marker_ready=0
last_marker_status="marker not found"
last_marker_error=""
for _ in $(seq 1 60); do
	find_output=""
	if find_output="$(docker exec "${node}" sh -c 'find /var/lib/opensandbox/nodeagent-data -path "*/kind-test/workloads/sb-normal/*/sandbox.finalized.*.json" -print' 2>"${find_stderr}")"; then
		find_status=0
	else
		find_status=$?
	fi
	if [[ ${find_status} -ne 0 ]]; then
		last_marker_status="marker lookup failed with status ${find_status}"
		last_marker_error="$(<"${find_stderr}")"
		sleep 2
		continue
	fi
	highest_marker_revision=-1
	marker=""
	while IFS= read -r candidate; do
		if [[ "${candidate}" =~ \.finalized\.([0-9]+)\.json$ ]] && (( BASH_REMATCH[1] > highest_marker_revision )); then
			highest_marker_revision="${BASH_REMATCH[1]}"
			marker="${candidate}"
		fi
	done <<<"${find_output}"
	if [[ -z "${marker}" ]]; then
		last_marker_status="marker not found"
		last_marker_error=""
		sleep 2
		continue
	fi

	marker_raw=""
	if marker_raw="$(docker exec "${node}" cat "${marker}" 2>"${marker_stderr}")"; then
		marker_read_status=0
	else
		marker_read_status=$?
	fi
	if [[ ${marker_read_status} -ne 0 ]]; then
		last_marker_status="marker read failed with status ${marker_read_status}"
		last_marker_error="$(<"${marker_stderr}")"
		sleep 2
		continue
	fi

	if jq -e '.resource.sandbox_id == "sb-normal" and .status == "incomplete" and .had_source_gaps == true and (.loss_reasons | index("monitor-interrupted")) != null and (.coverage_started_at | test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$")) and (.objects | length) >= 1' <<<"${marker_raw}" >/dev/null 2>"${jq_stderr}"; then
		marker_ready=1
		break
	else
		marker_query_status=$?
	fi
	if [[ ${marker_query_status} -eq 1 ]]; then
		last_marker_status="marker predicate not satisfied yet (${marker})"
	else
		last_marker_status="marker query failed with status ${marker_query_status} (${marker})"
	fi
	last_marker_error="$(<"${jq_stderr}")"
	if [[ -z "${last_marker_error}" ]]; then
		last_marker_error="marker content: ${marker_raw}"
	fi
	sleep 2
done

if [[ ${marker_ready} -ne 1 ]]; then
	echo "finalization marker did not become valid: ${last_marker_status}" >&2
	if [[ -n "${last_marker_error}" ]]; then
		echo "last marker error: ${last_marker_error}" >&2
	fi
	exit 1
fi

pool_check_ready=0
last_pool_status=0
last_pool_error=""
for _ in $(seq 1 10); do
	pool_output=""
	if pool_output="$(docker exec "${node}" grep -R -l -F -- "${pool_probe}" /var/lib/opensandbox/nodeagent-data 2>"${pool_stderr}")"; then
		pool_grep_status=0
	else
		pool_grep_status=$?
	fi
	if [[ -n "${pool_output}" ]]; then
		echo "Pool Pod unexpectedly produced file-sink output: ${pool_output}" >&2
		exit 1
	fi
	if [[ ${pool_grep_status} -eq 1 ]]; then
		pool_check_ready=1
		break
	fi
	last_pool_status=${pool_grep_status}
	last_pool_error="$(<"${pool_stderr}")"
	sleep 1
done
if [[ ${pool_check_ready} -ne 1 ]]; then
	echo "failed to inspect all node file-sink output after retries (status ${last_pool_status})" >&2
	if [[ -n "${last_pool_error}" ]]; then
		echo "last Pool exclusion check error: ${last_pool_error}" >&2
	fi
	exit 1
fi

pool_family=""
pool_family_ready=0
last_pool_family_status=0
last_pool_family_error=""
for _ in $(seq 1 10); do
	pool_family=""
	if pool_family="$(docker exec "${node}" sh -c 'find /var/lib/opensandbox/nodeagent-data \( -name .gc -o -name .quarantine \) -prune -o \( -path "*/kind-test/workloads/sb-pool" -o -path "*/kind-test/workloads/sb-pool/*" \) -print -quit' 2>"${pool_family_stderr}")"; then
		pool_family_find_status=0
	else
		pool_family_find_status=$?
	fi
	if [[ -n "${pool_family}" ]]; then
		echo "Pool Pod unexpectedly created a file-sink object family: ${pool_family}" >&2
		exit 1
	fi
	if [[ ${pool_family_find_status} -eq 0 && ! -s "${pool_family_stderr}" ]]; then
		pool_family_ready=1
		break
	fi
	last_pool_family_status=${pool_family_find_status}
	last_pool_family_error="$(<"${pool_family_stderr}")"
	sleep 1
done
if [[ ${pool_family_ready} -ne 1 ]]; then
	echo "failed to inspect file-sink output for a Pool Pod object family after retries (status ${last_pool_family_status}): ${last_pool_family_error}" >&2
	exit 1
fi

exit 0
