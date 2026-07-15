#!/bin/bash

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

# E2E benchmark: dns+nft vs dns+nft + transparent mitmproxy (HTTPS MITM).
# Two workload scenarios (configurable via BENCH_SCENARIOS):
#   short    — high concurrency short HTTPS requests (HEAD to many hosts)
#   download — large payload download (parallel GETs to one URL)
#
# Usage (from repo root or components/egress):
#   ./tests/bench-mitm-overhead.sh
# Optional:
#   SKIP_BUILD=1              — use existing ${IMG} image
#   BENCH_SAMPLE_SIZE=n       — sample n random domains from hostname.txt
#   BENCH_SCENARIOS=short,download  — comma-separated; default both
#   Short: BENCH_SHORT_INFLIGHT_PER_DOMAIN=n  — parallel HEADs per URL per round (default 1); raise for higher concurrency
#   Download: BENCH_DOWNLOAD_URL, BENCH_DOWNLOAD_PARALLEL, BENCH_DOWNLOAD_ROUNDS, BENCH_DOWNLOAD_MAXTIME
#   BENCH_DOCKER_STATS_INTERVAL=1  — sampling interval seconds for docker stats + /proc/loadavg (default 1)
#
# Requires: Docker, curl on host. Domain list: tests/hostname.txt.

set -euo pipefail

info() { echo "[$(date +%H:%M:%S)] $*"; }

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
HOSTNAME_FILE="${SCRIPT_DIR}/hostname.txt"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../../.." && pwd)"

IMG="opensandbox/egress:local"
CONTAINER_NAME="egress-bench-mitm"
POLICY_PORT=18080
ROUNDS=10
LOG_HOST_DIR="${LOG_HOST_DIR:-/tmp/egress-logs}"
LOG_FILE="${LOG_FILE:-egress.log}"
LOG_CONTAINER_DIR="/var/log/opensandbox"
LOG_CONTAINER_FILE="${LOG_CONTAINER_DIR}/${LOG_FILE}"

# Scenarios: short, download (comma-separated)
BENCH_SCENARIOS="${BENCH_SCENARIOS:-short,download}"
# Short: parallel HEAD requests per domain per round (1 = same as classic single-flight per URL)
BENCH_SHORT_INFLIGHT_PER_DOMAIN="${BENCH_SHORT_INFLIGHT_PER_DOMAIN:-1}"
# Download: one large HTTPS object, many parallel streams
BENCH_DOWNLOAD_URL="${BENCH_DOWNLOAD_URL:-https://speed.cloudflare.com/__down?bytes=20971520}"
BENCH_DOWNLOAD_PARALLEL="${BENCH_DOWNLOAD_PARALLEL:-4}"
BENCH_DOWNLOAD_ROUNDS="${BENCH_DOWNLOAD_ROUNDS:-1}"
BENCH_DOWNLOAD_MAXTIME="${BENCH_DOWNLOAD_MAXTIME:-600}"
BENCH_DOWNLOAD_EXEC_TIMEOUT="${BENCH_DOWNLOAD_EXEC_TIMEOUT:-900}"

if [[ ! -f "${HOSTNAME_FILE}" ]] || [[ ! -s "${HOSTNAME_FILE}" ]]; then
  echo "Error: domain file not found or empty: ${HOSTNAME_FILE}" >&2
  exit 1
fi
BENCH_DOMAINS=()
while IFS= read -r line; do
  line="${line%%#*}"
  line="${line#"${line%%[![:space:]]*}"}"
  line="${line%"${line##*[![:space:]]}"}"
  [[ -n "$line" ]] && BENCH_DOMAINS+=( "$line" )
done < "${HOSTNAME_FILE}"
total_in_file=${#BENCH_DOMAINS[@]}
if [[ "$total_in_file" -eq 0 ]]; then
  echo "Error: no domains in ${HOSTNAME_FILE}" >&2
  exit 1
fi

if [[ -n "${BENCH_SAMPLE_SIZE:-}" ]] && [[ "${BENCH_SAMPLE_SIZE}" -gt 0 ]]; then
  if [[ "${BENCH_SAMPLE_SIZE}" -ge "$total_in_file" ]]; then
    NUM_DOMAINS=$total_in_file
  else
    if command -v shuf >/dev/null 2>&1; then
      BENCH_DOMAINS=( $(printf '%s\n' "${BENCH_DOMAINS[@]}" | shuf -n "${BENCH_SAMPLE_SIZE}") )
    elif command -v gshuf >/dev/null 2>&1; then
      BENCH_DOMAINS=( $(printf '%s\n' "${BENCH_DOMAINS[@]}" | gshuf -n "${BENCH_SAMPLE_SIZE}") )
    else
      BENCH_DOMAINS=( $(printf '%s\n' "${BENCH_DOMAINS[@]}" | awk 'BEGIN{srand()} {printf "%s\t%s\n", rand(), $0}' | sort -n | cut -f2- | head -n "${BENCH_SAMPLE_SIZE}") )
    fi
    NUM_DOMAINS=${#BENCH_DOMAINS[@]}
    info "Using ${NUM_DOMAINS} randomly sampled domains (of ${total_in_file})"
  fi
else
  NUM_DOMAINS=$total_in_file
fi

TOTAL_REQUESTS=$((ROUNDS * NUM_DOMAINS * BENCH_SHORT_INFLIGHT_PER_DOMAIN))
CURL_TIMEOUT=10
BENCH_EXEC_TIMEOUT=300

# Hostname from https://host/path?query — for egress allow policy on download URL
download_url_host() {
  local u="$1"
  u="${u#http://}"
  u="${u#https://}"
  u="${u%%/*}"
  u="${u%%:*}"
  printf '%s\n' "$u"
}

BENCH_DOWNLOAD_HOST="$(download_url_host "${BENCH_DOWNLOAD_URL}")"

scenario_enabled() {
  local s="$1"
  echo ",${BENCH_SCENARIOS}," | grep -q ",${s},"
}

cleanup() {
  docker rm -f "${CONTAINER_NAME}" >/dev/null 2>&1 || true
}
trap cleanup EXIT

DOCKER_STATS_PID=""

start_docker_stats_log() {
  local outfile="$1"
  local interval="${BENCH_DOCKER_STATS_INTERVAL:-1}"
  (
    # load1/5/15 = cgroup view of load (same order as host loadavg; comparable under stress)
    echo -e "unix_ts\tload1\tload5\tload15\trunnable\ttotal_tasks\tCPUPerc\tMemUsage\tMemPerc\tNetIO\tBlockIO"
    while true; do
      ts=$(date +%s)
      la=$(docker exec "${CONTAINER_NAME}" cat /proc/loadavg 2>/dev/null) || la=""
      load1=""; load5=""; load15=""; rrun=""; rtot=""
      if [[ -n "$la" ]]; then
        read -r load1 load5 load15 rt_field _ <<< "$la"
        rrun="${rt_field%%/*}"
        rtot="${rt_field#*/}"
      fi
      line=$(docker stats "${CONTAINER_NAME}" --no-stream --format "{{.CPUPerc}}\t{{.MemUsage}}\t{{.MemPerc}}\t{{.NetIO}}\t{{.BlockIO}}" 2>/dev/null) || true
      [[ -n "$line" ]] && echo -e "${ts}\t${load1}\t${load5}\t${load15}\t${rrun}\t${rtot}\t${line}"
      sleep "${interval}"
    done
  ) >> "${outfile}" &
  DOCKER_STATS_PID=$!
}

stop_docker_stats_log() {
  if [[ -n "${DOCKER_STATS_PID}" ]]; then
    kill "${DOCKER_STATS_PID}" 2>/dev/null || true
    wait "${DOCKER_STATS_PID}" 2>/dev/null || true
    DOCKER_STATS_PID=""
  fi
}

stats() {
  local file="$1"
  if [[ ! -f "$file" ]] || [[ ! -s "$file" ]]; then
    echo "0 0 0 0"
    return
  fi
  sort -n "$file" > "${file}.sorted"
  local n
  n=$(wc -l < "${file}.sorted")
  if [[ "$n" -eq 0 ]]; then
    echo "0 0 0 0"
    return
  fi
  local avg p50 p99
  avg=$(awk '{s+=$1; c++} END { if(c>0) print s/c; else print 0 }' "$file")
  p50=$(awk -v n="$n" 'NR==int(n*0.5+0.5){print $1; exit}' "${file}.sorted")
  p99=$(awk -v n="$n" 'NR==int(n*0.99+0.5){print $1; exit}' "${file}.sorted")
  echo "$n $avg $p50 $p99"
}

# Column 1: time_total; stats on latency only
stats_download_time() {
  local file="$1"
  if [[ ! -f "$file" ]] || [[ ! -s "$file" ]]; then
    echo "0 0 0 0"
    return
  fi
  awk -F'\t' '{print $1}' "$file" | sort -n > "${file}.time.sorted"
  local n avg p50 p99
  n=$(wc -l < "${file}.time.sorted")
  if [[ "$n" -eq 0 ]]; then
    echo "0 0 0 0"
    return
  fi
  avg=$(awk '{s+=$1; c++} END { if(c>0) print s/c; else print 0 }' "${file}.time.sorted")
  p50=$(awk -v n="$n" 'NR==int(n*0.5+0.5){print $1; exit}' "${file}.time.sorted")
  p99=$(awk -v n="$n" 'NR==int(n*0.99+0.5){print $1; exit}' "${file}.time.sorted")
  echo "$n $avg $p50 $p99"
}

# End-to-end request latency loss vs baseline.
# Args: with_mitm_avg_seconds baseline_avg_seconds
# Echo: "<loss_ms_per_req> <loss_pct>"
calc_e2e_latency_loss() {
  local with_mitm_avg="$1"
  local baseline_avg="$2"
  local loss_ms loss_pct
  loss_ms=$(awk -v a="$with_mitm_avg" -v b="$baseline_avg" 'BEGIN { printf "%.2f", (a-b)*1000 }')
  loss_pct=$(awk -v a="$with_mitm_avg" -v b="$baseline_avg" 'BEGIN { if (b>0) printf "%+.2f", (a-b)/b*100; else print "+0.00" }')
  echo "$loss_ms $loss_pct"
}

# Sum of column 2 (size_download bytes)
sum_bytes() {
  local file="$1"
  awk -F'\t' '{s+=$2} END { print s+0 }' "$file" 2>/dev/null || echo "0"
}

run_bench_short() {
  local outfile="$1"
  local limit="${2:-9999}"
  local rounds="${3:-1}"
  local use_timeout="${4:-}"
  local inflight="${BENCH_SHORT_INFLIGHT_PER_DOMAIN:-1}"
  local cmd=(
    docker exec \
      -e BENCH_TIMEOUT="${CURL_TIMEOUT}" \
      -e BENCH_OUTFILE="${outfile}" \
      -e BENCH_LIMIT="${limit}" \
      -e BENCH_ROUNDS="${rounds}" \
      -e BENCH_INFLIGHT="${inflight}" \
      "${CONTAINER_NAME}" sh -c '
    : > "$BENCH_OUTFILE"
    r=1
    while [ "$r" -le "$BENCH_ROUNDS" ]; do
      n=0
      while IFS= read -r url && [ "$n" -lt "$BENCH_LIMIT" ]; do
        k=0
        while [ "$k" -lt "$BENCH_INFLIGHT" ]; do
          ( curl -o /dev/null -s -I -w "%{time_namelookup}\t%{time_total}\n" --max-time "$BENCH_TIMEOUT" "$url" >> "$BENCH_OUTFILE" ) &
          k=$((k+1))
        done
        n=$((n+1))
      done < /tmp/bench-domains.txt
      wait
      r=$((r+1))
    done
    '
  )
  if [[ "$use_timeout" == "timeout" ]] && command -v timeout >/dev/null 2>&1; then
    timeout "${BENCH_EXEC_TIMEOUT}" "${cmd[@]}"
  else
    "${cmd[@]}"
  fi
}

run_bench_download() {
  local outfile="$1"
  local use_timeout="$2"
  local url="${BENCH_DOWNLOAD_URL}"
  local par="${BENCH_DOWNLOAD_PARALLEL}"
  local drounds="${BENCH_DOWNLOAD_ROUNDS}"
  local maxt="${BENCH_DOWNLOAD_MAXTIME}"
  local cmd=(
    docker exec \
      -e BENCH_OUTFILE="${outfile}" \
      -e BENCH_DL_URL="${url}" \
      -e BENCH_DL_PAR="${par}" \
      -e BENCH_DL_ROUNDS="${drounds}" \
      -e BENCH_DL_MAX="${maxt}" \
      "${CONTAINER_NAME}" sh -c '
    : > "$BENCH_OUTFILE"
    r=1
    while [ "$r" -le "$BENCH_DL_ROUNDS" ]; do
      i=0
      while [ "$i" -lt "$BENCH_DL_PAR" ]; do
        ( curl -L -o /dev/null -s -w "%{time_total}\t%{size_download}\n" --max-time "$BENCH_DL_MAX" "$BENCH_DL_URL" >> "$BENCH_OUTFILE" ) &
        i=$((i+1))
      done
      wait
      r=$((r+1))
    done
    '
  )
  if [[ "$use_timeout" == "timeout" ]] && command -v timeout >/dev/null 2>&1; then
    timeout "${BENCH_DOWNLOAD_EXEC_TIMEOUT}" "${cmd[@]}"
  else
    "${cmd[@]}"
  fi
}

copy_url_file_to_container() {
  local url_file="/tmp/bench-mitm-domains-$$.txt"
  : > "${url_file}"
  for d in "${BENCH_DOMAINS[@]}"; do
    echo "https://${d}" >> "${url_file}"
  done
  docker cp "${url_file}" "${CONTAINER_NAME}:/tmp/bench-domains.txt"
  rm -f "${url_file}"
}

run_workload_short() {
  local mode="$1"
  local out_total="/tmp/bench-mitm-${mode}-short-total.txt"
  local out_namelookup="/tmp/bench-mitm-${mode}-short-namelookup.txt"
  : > "$out_total"
  : > "$out_namelookup"

  local first_url="https://${BENCH_DOMAINS[0]}"
  sleep 1
  if ! docker exec "${CONTAINER_NAME}" curl -o /dev/null -s -I --max-time "${CURL_TIMEOUT}" "${first_url}"; then
    info "Warm-up curl failed for ${first_url}"
    return 1
  fi

  info "Short workload: warm-up 10 domains × inflight=${BENCH_SHORT_INFLIGHT_PER_DOMAIN}..."
  bench_ret=0
  run_bench_short /tmp/bench-warmup.txt 10 1 2>/tmp/bench-mitm-stderr.txt || bench_ret=$?
  if [[ "$bench_ret" -ne 0 ]]; then
    info "Warm-up run failed (exit $bench_ret); continuing."
  fi

  info "Short workload: ${TOTAL_REQUESTS} HEAD requests (${ROUNDS} rounds × ${NUM_DOMAINS} URLs × ${BENCH_SHORT_INFLIGHT_PER_DOMAIN} inflight, max ${BENCH_EXEC_TIMEOUT}s)..."
  local start_ts
  start_ts=$(date +%s.%N)
  bench_ret=0
  run_bench_short /tmp/bench-raw.txt 9999 "${ROUNDS}" timeout 2>/tmp/bench-mitm-stderr.txt || bench_ret=$?
  if [[ "$bench_ret" -ne 0 ]]; then
    info "Short benchmark failed or timeout (exit $bench_ret); using partial results if any."
  fi
  docker cp "${CONTAINER_NAME}:/tmp/bench-raw.txt" /tmp/bench-mitm-short-raw.txt 2>/dev/null || true
  local end_ts
  end_ts=$(date +%s.%N)

  if [[ -s /tmp/bench-mitm-stderr.txt ]]; then
    head -5 /tmp/bench-mitm-stderr.txt >&2
  fi
  if [[ ! -f /tmp/bench-mitm-short-raw.txt ]]; then
    : > /tmp/bench-mitm-short-raw.txt
  fi
  awk -F'\t' '{print $2}' /tmp/bench-mitm-short-raw.txt 2>/dev/null > "$out_total"
  awk -F'\t' '{print $1}' /tmp/bench-mitm-short-raw.txt 2>/dev/null > "$out_namelookup"
  local wall_s
  wall_s=$(awk -v s="$start_ts" -v e="$end_ts" 'BEGIN { print e - s }')
  echo "$wall_s" > "/tmp/bench-mitm-${mode}-short-wall.txt"
}

run_workload_download() {
  local mode="$1"
  local out_raw="/tmp/bench-mitm-${mode}-download-raw.tsv"
  : > "$out_raw"

  info "Download workload: GET ${BENCH_DOWNLOAD_URL}"
  info "  parallel=${BENCH_DOWNLOAD_PARALLEL} rounds=${BENCH_DOWNLOAD_ROUNDS} maxtime=${BENCH_DOWNLOAD_MAXTIME}s (wall cap ${BENCH_DOWNLOAD_EXEC_TIMEOUT}s)"

  if ! docker exec "${CONTAINER_NAME}" curl -o /dev/null -sf -I --max-time "${CURL_TIMEOUT}" "${BENCH_DOWNLOAD_URL}"; then
    info "Warm-up HEAD to download URL failed; trying full GET once..."
    docker exec "${CONTAINER_NAME}" curl -L -o /dev/null -sf --max-time 30 "${BENCH_DOWNLOAD_URL}" || {
      info "ERROR: cannot reach download URL from container"
      return 1
    }
  fi

  local start_ts end_ts bench_ret
  start_ts=$(date +%s.%N)
  bench_ret=0
  run_bench_download /tmp/bench-dl-raw.txt timeout 2>/tmp/bench-mitm-dl-stderr.txt || bench_ret=$?
  end_ts=$(date +%s.%N)
  docker cp "${CONTAINER_NAME}:/tmp/bench-dl-raw.txt" /tmp/bench-mitm-dl-raw-host.txt 2>/dev/null || true
  if [[ "$bench_ret" -ne 0 ]]; then
    info "Download benchmark failed or timeout (exit $bench_ret); using partial results if any."
  fi
  if [[ -s /tmp/bench-mitm-dl-stderr.txt ]]; then
    head -5 /tmp/bench-mitm-dl-stderr.txt >&2
  fi
  if [[ -f /tmp/bench-mitm-dl-raw-host.txt ]]; then
    cp /tmp/bench-mitm-dl-raw-host.txt "$out_raw"
  else
    : > "$out_raw"
  fi

  awk -F'\t' '{print $1}' "$out_raw" 2>/dev/null > "/tmp/bench-mitm-${mode}-download-time.txt"
  local wall_s
  wall_s=$(awk -v s="$start_ts" -v e="$end_ts" 'BEGIN { print e - s }')
  echo "$wall_s" > "/tmp/bench-mitm-${mode}-download-wall.txt"
  local total_bytes
  total_bytes=$(sum_bytes "$out_raw")
  echo "$total_bytes" > "/tmp/bench-mitm-${mode}-download-bytes.txt"
}

run_workloads_for_mode() {
  local mode="$1"
  if scenario_enabled short; then
    run_workload_short "${mode}"
  fi
  if scenario_enabled download; then
    run_workload_download "${mode}"
  fi
}

# Args: mode label (dns_nft | dns_nft_mitm), enable mitm (0|1)
run_phase() {
  local mode="$1"
  local with_mitm="$2"
  info "Phase: dns+nft (mitm=${with_mitm})"
  cleanup
  mkdir -p "${LOG_HOST_DIR}"

  local docker_env=(
    -e OPENSANDBOX_EGRESS_MODE=dns+nft
    -e OPENSANDBOX_LOG_OUTPUT="${LOG_CONTAINER_FILE}"
  )
  if [[ "$with_mitm" == "1" ]]; then
    docker_env+=(
      -e OPENSANDBOX_EGRESS_MITMPROXY_TRANSPARENT=true
      -e OPENSANDBOX_EGRESS_MITMPROXY_PORT="${OPENSANDBOX_EGRESS_MITMPROXY_PORT:-18081}"
    )
  fi

  docker run -d --name "${CONTAINER_NAME}" \
    --cap-add=NET_ADMIN \
    --sysctl net.ipv6.conf.all.disable_ipv6=1 \
    --sysctl net.ipv6.conf.default.disable_ipv6=1 \
    "${docker_env[@]}" \
    -v "${LOG_HOST_DIR}:${LOG_CONTAINER_DIR}" \
    -p "${POLICY_PORT}:18080" \
    "${IMG}"

  for i in $(seq 1 30); do
    if curl -sf "http://127.0.0.1:${POLICY_PORT}/healthz" >/dev/null 2>&1; then
      break
    fi
    sleep 0.5
  done

  # Apply allow policy before any outbound HTTPS probe. Default-deny would block curl until /policy is posted.
  local policy_hosts=( "${BENCH_DOMAINS[@]}" )
  if scenario_enabled download && [[ -n "${BENCH_DOWNLOAD_HOST}" ]]; then
    local found=0
    for h in "${policy_hosts[@]}"; do
      [[ "$h" == "${BENCH_DOWNLOAD_HOST}" ]] && found=1 && break
    done
    if [[ "$found" -eq 0 ]]; then
      policy_hosts+=( "${BENCH_DOWNLOAD_HOST}" )
      info "Policy: allowing download host ${BENCH_DOWNLOAD_HOST}"
    fi
  fi

  local policy_egress=""
  for d in "${policy_hosts[@]}"; do
    policy_egress="${policy_egress}{\"action\":\"allow\",\"target\":\"${d}\"},"
  done
  policy_egress="${policy_egress%,}"
  local policy_json="{\"defaultAction\":\"deny\",\"egress\":[${policy_egress}]}"
  curl -sf -XPOST "http://127.0.0.1:${POLICY_PORT}/policy" -d "${policy_json}" >/dev/null

  copy_url_file_to_container

  if [[ "$with_mitm" == "1" ]]; then
    info "Waiting for mitmdump + CA trust (up to ~90s, policy already allows ${BENCH_DOMAINS[0]})..."
    local ok=0
    for i in $(seq 1 90); do
      if docker exec "${CONTAINER_NAME}" curl -o /dev/null -sf -I --max-time "${CURL_TIMEOUT}" "https://${BENCH_DOMAINS[0]}" 2>/dev/null; then
        ok=1
        break
      fi
      sleep 1
    done
    if [[ "$ok" -ne 1 ]]; then
      info "WARN: HTTPS to first domain did not succeed in time; benchmark may fail (check logs)."
    fi
  fi
  local stats_out="/tmp/bench-mitm-docker-stats-${mode}.tsv"
  : > "${stats_out}"
  info "Container metrics (docker stats + /proc/loadavg) -> ${stats_out} (interval ${BENCH_DOCKER_STATS_INTERVAL:-1}s)"
  start_docker_stats_log "${stats_out}"
  run_workloads_for_mode "${mode}"
  stop_docker_stats_log
  info "Docker stats log: ${stats_out}"
}

report_short() {
  local n0 n1 avg0 avg1 p50_0 p50_1 p99_0 p99_1 wall0 wall1 rps0 rps1
  read -r n0 avg0 p50_0 p99_0 <<< "$(stats /tmp/bench-mitm-dns_nft-short-total.txt)"
  read -r n1 avg1 p50_1 p99_1 <<< "$(stats /tmp/bench-mitm-dns_nft_mitm-short-total.txt)"
  wall0=$(cat /tmp/bench-mitm-dns_nft-short-wall.txt 2>/dev/null || echo "0")
  wall1=$(cat /tmp/bench-mitm-dns_nft_mitm-short-wall.txt 2>/dev/null || echo "0")
  rps0=$(awk -v n="$n0" -v w="$wall0" 'BEGIN { print (w>0 && n>0) ? n/w : 0 }')
  rps1=$(awk -v n="$n1" -v w="$wall1" 'BEGIN { print (w>0 && n>0) ? n/w : 0 }')

  local ov_avg ov_p50 ov_p99 ov_rps
  ov_avg=$(awk -v a="$avg1" -v b="$avg0" 'BEGIN { printf "%+.1f", (b>0 && b!="") ? (a-b)/b*100 : 0 }')
  ov_p50=$(awk -v a="$p50_1" -v b="$p50_0" 'BEGIN { printf "%+.1f", (b>0 && b!="") ? (a-b)/b*100 : 0 }')
  ov_p99=$(awk -v a="$p99_1" -v b="$p99_0" 'BEGIN { printf "%+.1f", (b>0 && b!="") ? (a-b)/b*100 : 0 }')
  ov_rps=$(awk -v a="$rps1" -v b="$rps0" 'BEGIN { printf "%+.1f", (b>0 && b!="") ? (b-a)/b*100 : 0 }')
  local e2e_loss_ms e2e_loss_pct
  read -r e2e_loss_ms e2e_loss_pct <<< "$(calc_e2e_latency_loss "$avg1" "$avg0")"

  echo ""
  echo "========== Scenario: short HTTPS (HEAD), high concurrency =========="
  echo "Config: ${ROUNDS} rounds × ${NUM_DOMAINS} URLs × ${BENCH_SHORT_INFLIGHT_PER_DOMAIN} inflight/URL → ${TOTAL_REQUESTS} requests"
  echo ""
  printf "%-28s %14s %20s %20s %20s\n" "Mode" "Req/s" "Avg(s)" "P50(s)" "P99(s)"
  printf "%-28s %14s %20s %20s %20s\n" "dns+nft (no mitm)" "$rps0" "$avg0" "$p50_0" "$p99_0"
  printf "%-28s %14s %20s %20s %20s\n" "dns+nft + mitm (transparent)" "$(printf '%.2f(%s%%)' "$rps1" "$ov_rps")" "$(printf '%.3f(%s%%)' "$avg1" "$ov_avg")" "$(printf '%.3f(%s%%)' "$p50_1" "$ov_p50")" "$(printf '%.3f(%s%%)' "$p99_1" "$ov_p99")"
  echo ""
  echo "E2E latency loss (avg time_total): ${e2e_loss_ms} ms/request (${e2e_loss_pct}%)"
  echo "Parentheses vs dns+nft (no mitm): latency +% = slower; Req/s +% = lower throughput."
  echo "Artifacts: /tmp/bench-mitm-dns_nft-{short-total,short-wall}.txt and *-dns_nft_mitm-short-*.txt"
  echo "=========="
}

report_download() {
  local n0 n1 avg0 avg1 p50_0 p50_1 p99_0 p99_1 wall0 wall1 bytes0 bytes1 mbps0 mbps1 mib0 mib1
  read -r n0 avg0 p50_0 p99_0 <<< "$(stats_download_time /tmp/bench-mitm-dns_nft-download-raw.tsv)"
  read -r n1 avg1 p50_1 p99_1 <<< "$(stats_download_time /tmp/bench-mitm-dns_nft_mitm-download-raw.tsv)"
  wall0=$(cat /tmp/bench-mitm-dns_nft-download-wall.txt 2>/dev/null || echo "0")
  wall1=$(cat /tmp/bench-mitm-dns_nft_mitm-download-wall.txt 2>/dev/null || echo "0")
  bytes0=$(cat /tmp/bench-mitm-dns_nft-download-bytes.txt 2>/dev/null || echo "0")
  bytes1=$(cat /tmp/bench-mitm-dns_nft_mitm-download-bytes.txt 2>/dev/null || echo "0")
  mbps0=$(awk -v b="$bytes0" -v w="$wall0" 'BEGIN { if (w>0 && b>0) printf "%.2f", (b/1048576)/w; else print "0" }')
  mbps1=$(awk -v b="$bytes1" -v w="$wall1" 'BEGIN { if (w>0 && b>0) printf "%.2f", (b/1048576)/w; else print "0" }')
  mib0=$(awk -v b="$bytes0" 'BEGIN{ printf "%.1f", b/1048576 }')
  mib1=$(awk -v b="$bytes1" 'BEGIN{ printf "%.1f", b/1048576 }')

  local ov_avg ov_p50 ov_p99 ov_mbps
  ov_avg=$(awk -v a="$avg1" -v b="$avg0" 'BEGIN { printf "%+.1f", (b>0 && b!="") ? (a-b)/b*100 : 0 }')
  ov_p50=$(awk -v a="$p50_1" -v b="$p50_0" 'BEGIN { printf "%+.1f", (b>0 && b!="") ? (a-b)/b*100 : 0 }')
  ov_p99=$(awk -v a="$p99_1" -v b="$p99_0" 'BEGIN { printf "%+.1f", (b>0 && b!="") ? (a-b)/b*100 : 0 }')
  ov_mbps=$(awk -v a="$mbps1" -v b="$mbps0" 'BEGIN { printf "%+.1f", (b>0 && b!="") ? (b-a)/b*100 : 0 }')
  local e2e_loss_ms e2e_loss_pct
  read -r e2e_loss_ms e2e_loss_pct <<< "$(calc_e2e_latency_loss "$avg1" "$avg0")"

  echo ""
  echo "========== Scenario: large download (parallel GET) =========="
  echo "URL: ${BENCH_DOWNLOAD_URL}"
  echo "Streams: ${BENCH_DOWNLOAD_PARALLEL} parallel × ${BENCH_DOWNLOAD_ROUNDS} round(s)"
  echo ""
  printf "%-28s %18s %20s %20s %20s %12s\n" "Mode" "Agg MB/s" "Avg(s)/stream" "P50(s)" "P99(s)" "Total MiB"
  printf "%-28s %18s %20s %20s %20s %12s\n" "dns+nft (no mitm)" "${mbps0}" "$avg0" "$p50_0" "$p99_0" "$mib0"
  printf "%-28s %18s %20s %20s %20s %12s\n" "dns+nft + mitm (transparent)" "$(printf '%.2f(%s%%)' "$mbps1" "$ov_mbps")" "$(printf '%.3f(%s%%)' "$avg1" "$ov_avg")" "$(printf '%.3f(%s%%)' "$p50_1" "$ov_p50")" "$(printf '%.3f(%s%%)' "$p99_1" "$ov_p99")" "$mib1"
  echo ""
  echo "E2E latency loss (avg time_total): ${e2e_loss_ms} ms/request (${e2e_loss_pct}%)"
  echo "Agg MB/s = sum(bytes) / wall clock for the download phase (overlapping streams). Latency % vs no mitm; Agg MB/s % = lower throughput."
  echo "Artifacts: /tmp/bench-mitm-*-download-{raw.tsv,wall,bytes}.txt"
  echo "=========="
}

report() {
  if scenario_enabled short; then
    report_short
  fi
  if scenario_enabled download; then
    report_download
  fi
  echo ""
  echo "Container metrics TSV: /tmp/bench-mitm-docker-stats-dns_nft.tsv and *-dns_nft_mitm.tsv (load1/5/15 + docker stats)"
}

if [[ -z "${SKIP_BUILD:-}" ]]; then
  info "Building image ${IMG}"
  docker build -t "${IMG}" -f "${REPO_ROOT}/components/egress/Dockerfile" "${REPO_ROOT}" > /dev/null 2>&1
else
  info "SKIP_BUILD=1: using existing ${IMG}"
fi

info "Scenarios: ${BENCH_SCENARIOS}"
run_phase "dns_nft" 0
run_phase "dns_nft_mitm" 1
report
info "Done"
cleanup
