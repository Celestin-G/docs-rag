#!/usr/bin/env bash
# Read-only verification of the metadata filters of a Bedrock Knowledge Base (S3 Vectors),
# to run AFTER a real publish (rag publish). It only calls `aws bedrock-agent-runtime retrieve`
# (and `aws bedrock-agent get-knowledge-base` with --preflight). Nothing is written.
#
# Usage:
#   AWS_PROFILE=formance-internal-bedrock scripts/kb-verify.sh --kb-id KB_ID [--stack-version 3.1] [--results 10] [--region eu-west-1] [--preflight]
#
# Options (flags override env):
#   --kb-id ID            Knowledge Base id (env KB_ID, required, never hardcoded)
#   --region REGION       default eu-west-1 (env AWS_REGION); any non eu-* region is refused
#   --results N           results per Retrieve call, 1..100 (default 10)
#   --stack-version X.Y   Stack version for the numeric tests (default 3.1 = 301, scale major*100+minor)
#   --baseline-query Q    test 1 query   (env KB_VERIFY_BASELINE_QUERY)
#   --cloud-query Q       test 2 query   (env KB_VERIFY_CLOUD_QUERY)
#   --ee-query Q         test 3 query   (env KB_VERIFY_EE_QUERY)
#   --preflight           also check the KB exists with `get-knowledge-base`
#
# Tests (PASS/FAIL, INFO is informational):
#   1  numeric filter  baseline (client_id only) shows content with version_min > X; then
#                      andAll [client_id, version_min <= X, version_below > X] for X and X+1:
#                      FAIL if the API rejects numeric filters or a result is out of range
#   2  deployment      cloud topic: deployment in [cloud, any] vs [self-hosted, any]
#   3  license         enterprise topic: license in [ee, any] vs [oss, any]
#   4  combined        the bot's real filter "Stack vX.Y, cloud, EE" (5 conditions)
#
# EVERY Retrieve carries client_id in ["none"] (see build_filter). Relevance of the results
# is left to the reader: each result is printed with score, source_url and its metadata.
# Exit code: 0 when no FAIL, 1 on any FAIL, 2 on a configuration error.
#
# Requires: aws CLI v2, jq.
set -euo pipefail

readonly DEFAULT_REGION="eu-west-1"
readonly DEFAULT_RESULTS=10
readonly DEFAULT_STACK_VERSION="3.1"
readonly DEFAULT_BASELINE_QUERY="payments connectors capabilities and versions"
readonly DEFAULT_CLOUD_QUERY="How do I choose a region for my Formance Cloud stack?"
readonly DEFAULT_EE_QUERY="How do I configure SSO with an external identity provider?"

PASS_COUNT=0
FAIL_COUNT=0
INFO_COUNT=0
PREFLIGHT=0
RESULTS_N="${DEFAULT_RESULTS}"
STACK_VERSION="${DEFAULT_STACK_VERSION}"
BASELINE_QUERY="${KB_VERIFY_BASELINE_QUERY:-${DEFAULT_BASELINE_QUERY}}"
CLOUD_QUERY="${KB_VERIFY_CLOUD_QUERY:-${DEFAULT_CLOUD_QUERY}}"
EE_QUERY="${KB_VERIFY_EE_QUERY:-${DEFAULT_EE_QUERY}}"
KB_ID="${KB_ID:-}"
CLI_REGION=""
ERR_FILE=""
RESULTS_JSON="[]"
RETRIEVE_ERR=""
NUMERIC_EMPTY_LABELS=()

# ---------- logging ----------
log() { printf '%s\n' "$*"; }
pass() {
  PASS_COUNT=$((PASS_COUNT + 1))
  log "PASS: $*"
}
fail() {
  FAIL_COUNT=$((FAIL_COUNT + 1))
  log "FAIL: $*"
}
info() {
  INFO_COUNT=$((INFO_COUNT + 1))
  log "INFO: $*"
}
die_config() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 2
}

usage() {
  sed -n '2,/^# Requires/p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//' >&2
}

# ---------- args and config ----------
need_value() { [ "$1" -ge 2 ] || die_config "$2 needs a value"; }

parse_args() {
  while [ $# -gt 0 ]; do
    case "$1" in
      --kb-id) need_value $# "$1"; KB_ID="$2"; shift ;;
      --region) need_value $# "$1"; CLI_REGION="$2"; shift ;;
      --results) need_value $# "$1"; RESULTS_N="$2"; shift ;;
      --stack-version) need_value $# "$1"; STACK_VERSION="$2"; shift ;;
      --baseline-query) need_value $# "$1"; BASELINE_QUERY="$2"; shift ;;
      --cloud-query) need_value $# "$1"; CLOUD_QUERY="$2"; shift ;;
      --ee-query) need_value $# "$1"; EE_QUERY="$2"; shift ;;
      --preflight) PREFLIGHT=1 ;;
      -h | --help)
        usage
        exit 0
        ;;
      *)
        usage
        die_config "unknown option: $1"
        ;;
    esac
    shift
  done
}

check_config() {
  AWS_REGION="${CLI_REGION:-${AWS_REGION:-${DEFAULT_REGION}}}"
  case "${AWS_REGION}" in
    eu-*) ;;
    *) die_config "region '${AWS_REGION}' refused: only eu-* regions are allowed" ;;
  esac
  export AWS_REGION AWS_DEFAULT_REGION="${AWS_REGION}"
  [ -n "${KB_ID}" ] || die_config "KB_ID is required (--kb-id or env KB_ID)"
  case "${RESULTS_N}" in
    '' | *[!0-9]*) die_config "--results must be an integer between 1 and 100, got '${RESULTS_N}'" ;;
  esac
  if [ "${RESULTS_N}" -lt 1 ] || [ "${RESULTS_N}" -gt 100 ]; then
    die_config "--results must be between 1 and 100, got '${RESULTS_N}'"
  fi
  if ! [[ "${STACK_VERSION}" =~ ^[0-9]+\.[0-9]+$ ]]; then
    die_config "--stack-version must look like 3.1, got '${STACK_VERSION}'"
  fi
  # scale is major*100+minor: a minor above 99 would collide with the next major
  local minor="${STACK_VERSION#*.}"
  minor="${minor#"${minor%%[!0]*}"}"
  if [ "${#minor}" -gt 2 ]; then
    die_config "--stack-version minor must be <= 99 (scale is major*100+minor), got '${STACK_VERSION}'"
  fi
  command -v jq >/dev/null 2>&1 || die_config "jq is required"
  command -v aws >/dev/null 2>&1 || die_config "aws CLI v2 is required"
}

# stack_scale X.Y -> major*100+minor (3.1 -> 301)
stack_scale() {
  local major="${1%%.*}" minor="${1#*.}"
  printf '%s' "$((10#${major} * 100 + 10#${minor}))"
}

# ---------- filters ----------
# Condition builders: each prints one compact JSON condition.
c_in() { jq -nc --arg k "$1" --args '{in: {key: $k, value: $ARGS.positional}}' "${@:2}"; }
c_num() { jq -nc --arg op "$1" --arg k "$2" --argjson v "$3" '{($op): {key: $k, value: $v}}'; }

# build_filter [CONDITIONS_JSON_ARRAY]: the ONLY way a filter is made, so no call can omit
# client_id in ["none"]. andAll needs >= 2 conditions: with no extra condition the bare `in`
# is returned, not wrapped.
build_filter() {
  jq -nc --argjson extra "${1:-[]}" '
    {in: {key: "client_id", value: ["none"]}} as $cid
    | if ($extra | length) == 0 then $cid else {andAll: ([$cid] + $extra)} end'
}

# ---------- retrieve ----------
# retrieve QUERY CONDITIONS_JSON_ARRAY: sets RESULTS_JSON (normalized array) or RETRIEVE_ERR.
# Numbers may come back as numbers or strings: both are converted with tonumber.
retrieve() {
  local query="$1" filter cfg q out
  filter="$(build_filter "${2:-[]}")"
  cfg="$(jq -nc --argjson n "${RESULTS_N}" --argjson f "${filter}" '{vectorSearchConfiguration: {numberOfResults: $n, filter: $f}}')"
  q="$(jq -nc --arg q "${query}" '{text: $q}')"
  RESULTS_JSON="[]"
  RETRIEVE_ERR=""
  if ! out="$(aws bedrock-agent-runtime retrieve --knowledge-base-id "${KB_ID}" \
    --retrieval-query "${q}" --retrieval-configuration "${cfg}" \
    --region "${AWS_REGION}" --output json 2>"${ERR_FILE}")"; then
    RETRIEVE_ERR="$(cat "${ERR_FILE}")"
    [ -n "${RETRIEVE_ERR}" ] || RETRIEVE_ERR="aws exited with a non-zero status"
    return 1
  fi
  RESULTS_JSON="$(jq -c '
    def n: if type == "number" then . elif type == "string" then (tonumber? // null) else null end;
    [.retrievalResults[]? | (.metadata // {}) as $m | {
      score: (.score // null),
      uri: (.location.s3Location.uri // ""),
      url: ($m.source_url // ""),
      m: {
        product: ($m.product // null), deployment: ($m.deployment // null),
        license: ($m.license // null), client_id: ($m.client_id // null),
        version_min: ($m.version_min | n), version_below: ($m.version_below | n)}}]' <<<"${out}")" || {
    RETRIEVE_ERR="could not parse the Retrieve response"
    return 1
  }
}

# print_results: readable listing of RESULTS_JSON.
print_results() {
  if [ "$(jq 'length' <<<"${RESULTS_JSON}")" -eq 0 ]; then
    log "    (no results)"
    return 0
  fi
  jq -r 'to_entries[] | .key as $i | .value | "    [\($i + 1)] score=\(.score // "?") product=\(.m.product // "?") deployment=\(.m.deployment // "?") license=\(.m.license // "?") version_min=\(.m.version_min // "?") version_below=\(.m.version_below // "?")\n        \(if .url == "" then .uri else .url end)"' <<<"${RESULTS_JSON}"
}

# violations EXPR [X]: JSON array of RESULTS_JSON entries matching the jq violation expression.
violations() {
  jq -c --argjson x "${2:-0}" "[.[] | select(${1})]" <<<"${RESULTS_JSON}"
}

# report_violations LABEL EXPR [X]: PASS/FAIL/INFO for the current RESULTS_JSON.
report_violations() {
  local label="$1" bad total
  bad="$(violations "$2" "${3:-0}")"
  total="$(jq 'length' <<<"${RESULTS_JSON}")"
  if [ "$(jq 'length' <<<"${bad}")" -gt 0 ]; then
    fail "${label}: $(jq 'length' <<<"${bad}") of ${total} results violate the filter"
    jq -r '.[] | "    violation: \(.url) deployment=\(.m.deployment) license=\(.m.license) version_min=\(.m.version_min) version_below=\(.m.version_below) client_id=\(.m.client_id)"' <<<"${bad}"
  elif [ "${total}" -eq 0 ]; then
    info "${label}: 0 results, nothing to check (inconclusive)"
  else
    pass "${label}: ${total} results, all satisfy the filter"
  fi
}

# run_case LABEL QUERY CONDITIONS VIOLATION_EXPR [X]: one filtered Retrieve, printed and checked.
run_case() {
  local label="$1" query="$2" conds="$3" expr="$4" x="${5:-0}"
  log ""
  log "--- ${label}"
  log "    query: ${query}"
  log "    filter: $(build_filter "${conds}")"
  if ! retrieve "${query}" "${conds}"; then
    fail "${label}: retrieve call failed: ${RETRIEVE_ERR}"
    return 0
  fi
  print_results
  report_violations "${label}" "${expr}" "${x}"
}

# ---------- tests ----------
test_numeric() {
  local x="$1" label="$2" defer_empty="${3:-0}" conds expr
  conds="$(jq -nc --argjson x "${x}" '[{lessThanOrEquals: {key: "version_min", value: $x}}, {greaterThan: {key: "version_below", value: $x}}]')"
  expr='(.m.client_id != "none") or (.m.version_min == null) or (.m.version_min > $x) or (.m.version_below == null) or (.m.version_below <= $x)'
  log ""
  log "--- ${label}: andAll [client_id, version_min <= ${x}, version_below > ${x}]"
  log "    query: ${BASELINE_QUERY}"
  log "    filter: $(build_filter "${conds}")"
  if ! retrieve "${BASELINE_QUERY}" "${conds}"; then
    if grep -Eqi 'ValidationException|filter|operator' <<<"${RETRIEVE_ERR}"; then
      log "=============================================================================="
      log "NUMERIC FILTERS NOT SUPPORTED: use the fallback in docs/kb-contract-changes.md"
      log "=============================================================================="
      log "    error: ${RETRIEVE_ERR}"
      fail "${label}: numeric filter rejected by the API"
    else
      fail "${label}: retrieve failed: ${RETRIEVE_ERR}"
    fi
    return 0
  fi
  print_results
  if [ "${defer_empty}" -eq 1 ] && [ "$(jq 'length' <<<"${RESULTS_JSON}")" -eq 0 ]; then
    NUMERIC_EMPTY_LABELS+=("${label}")
    return 0
  fi
  report_violations "${label}" "${expr}" "${x}"
}

test_1_numeric() {
  local x x2 later total matching defer=0 l
  x="$(stack_scale "${STACK_VERSION}")"
  x2=$((x + 1))
  log ""
  log "=== Test 1: numeric filter (Stack v${STACK_VERSION} = ${x})"
  log "--- 1a baseline (client_id only)"
  log "    query: ${BASELINE_QUERY}"
  log "    filter: $(build_filter '[]')"
  if retrieve "${BASELINE_QUERY}" '[]'; then
    print_results
    total="$(jq 'length' <<<"${RESULTS_JSON}")"
    later="$(jq --argjson x "${x}" '[.[] | select(.m.version_min != null and .m.version_min > $x)] | length' <<<"${RESULTS_JSON}")"
    matching="$(jq --argjson x "${x}" --argjson x2 "$((x + 1))" '[.[] | select(.m.version_min != null and .m.version_below != null and ((.m.version_min <= $x and $x < .m.version_below) or (.m.version_min <= $x2 and $x2 < .m.version_below)))] | length' <<<"${RESULTS_JSON}")"
    [ "${matching}" -gt 0 ] && defer=1
    if [ "${later}" -gt 0 ]; then
      info "1a baseline: ${later} of ${total} results have version_min > ${x} (content for later versions exists, so the numeric filter has something to exclude)"
    else
      info "1a baseline: 0 of ${total} results have version_min > ${x}: the numeric check is INCONCLUSIVE, try another query (--baseline-query) that matches version-specific content"
    fi
  else
    fail "1a baseline: retrieve call failed: ${RETRIEVE_ERR}"
  fi
  NUMERIC_EMPTY_LABELS=()
  test_numeric "${x}" "1b X=${x}" "${defer}"
  test_numeric "${x2}" "1c X=${x2}" "${defer}"
  if [ "${#NUMERIC_EMPTY_LABELS[@]}" -eq 2 ]; then
    fail "1b/1c: numeric filter matched nothing although matching content exists; numbers may be stored as strings"
  else
    for l in ${NUMERIC_EMPTY_LABELS[@]+"${NUMERIC_EMPTY_LABELS[@]}"}; do
      info "${l}: 0 results, nothing to check (inconclusive)"
    done
  fi
}

test_2_deployment() {
  local c1 c2 cloud
  log ""
  log "=== Test 2: deployment (cloud topic)"
  c1="$(c_in deployment cloud any)"
  c2="$(c_in deployment self-hosted any)"
  run_case "2a deployment in [cloud, any]" "${CLOUD_QUERY}" "[${c1}]" \
    '(.m.client_id != "none") or (.m.deployment | IN("cloud", "any") | not)'
  cloud="$(jq -c '[.[] | select(.m.deployment == "cloud")]' <<<"${RESULTS_JSON}")"
  if [ "$(jq 'length' <<<"${cloud}")" -gt 0 ]; then
    info "2a: $(jq 'length' <<<"${cloud}") results with deployment=cloud (expected > 0): $(jq -r '[.[].url] | join(", ")' <<<"${cloud}")"
  else
    info "2a: 0 results with deployment=cloud (expected > 0): check that cloud-only content is published and the query matches it"
  fi
  run_case "2b deployment in [self-hosted, any]" "${CLOUD_QUERY}" "[${c2}]" \
    '(.m.client_id != "none") or (.m.deployment | IN("self-hosted", "any") | not)'
}

test_3_license() {
  local c1 c2 ee
  log ""
  log "=== Test 3: license (enterprise topic)"
  c1="$(c_in license ee any)"
  c2="$(c_in license oss any)"
  run_case "3a license in [ee, any]" "${EE_QUERY}" "[${c1}]" \
    '(.m.client_id != "none") or (.m.license | IN("ee", "any") | not)'
  ee="$(jq -c '[.[] | select(.m.license == "ee")]' <<<"${RESULTS_JSON}")"
  if [ "$(jq 'length' <<<"${ee}")" -gt 0 ]; then
    info "3a: $(jq 'length' <<<"${ee}") results with license=ee (expected > 0): $(jq -r '[.[].url] | join(", ")' <<<"${ee}")"
  else
    info "3a: 0 results with license=ee (expected > 0): check that enterprise content is published and the query matches it"
  fi
  run_case "3b license in [oss, any]" "${EE_QUERY}" "[${c2}]" \
    '(.m.client_id != "none") or (.m.license | IN("oss", "any") | not)'
}

test_4_combined() {
  local x conds expr
  x="$(stack_scale "${STACK_VERSION}")"
  log ""
  log "=== Test 4: combined bot filter (Stack v${STACK_VERSION}, cloud, EE)"
  conds="$(jq -nc --argjson x "${x}" '[
    {lessThanOrEquals: {key: "version_min", value: $x}},
    {greaterThan: {key: "version_below", value: $x}},
    {in: {key: "deployment", value: ["cloud", "any"]}},
    {in: {key: "license", value: ["ee", "any"]}}]')"
  expr='(.m.client_id != "none") or (.m.version_min == null) or (.m.version_min > $x) or (.m.version_below == null) or (.m.version_below <= $x) or (.m.deployment | IN("cloud", "any") | not) or (.m.license | IN("ee", "any") | not)'
  run_case "4 combined" "${CLOUD_QUERY}" "${conds}" "${expr}" "${x}"
}

preflight() {
  local out
  if out="$(aws bedrock-agent get-knowledge-base --knowledge-base-id "${KB_ID}" --region "${AWS_REGION}" --output json 2>"${ERR_FILE}")"; then
    pass "preflight: knowledge base ${KB_ID} exists ($(jq -r '.knowledgeBase.name // "?"' <<<"${out}"), region ${AWS_REGION})"
  else
    fail "preflight: knowledge base ${KB_ID} not reachable: $(cat "${ERR_FILE}")"
    summary
    exit 1
  fi
}

summary() {
  log ""
  log "----"
  log "SUMMARY: ${PASS_COUNT} PASS, ${FAIL_COUNT} FAIL, ${INFO_COUNT} INFO (kb ${KB_ID}, ${AWS_REGION}, Stack v${STACK_VERSION})"
  log "Relevance of the results above is for a human to judge."
}

main() {
  parse_args "$@"
  check_config
  ERR_FILE="$(mktemp)"
  trap 'rm -f "${ERR_FILE}"' EXIT
  log "kb-verify: KB ${KB_ID}, region ${AWS_REGION}, profile ${AWS_PROFILE:-default}, ${RESULTS_N} results per call (read-only)"
  if [ "${PREFLIGHT}" -eq 1 ]; then preflight; fi
  test_1_numeric
  test_2_deployment
  test_3_license
  test_4_combined
  summary
  [ "${FAIL_COUNT}" -eq 0 ]
}

main "$@"
