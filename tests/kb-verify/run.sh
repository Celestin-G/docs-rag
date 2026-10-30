#!/usr/bin/env bash
# Tests for scripts/kb-verify.sh using a fake `aws` (no AWS access needed).
# Usage: tests/kb-verify/run.sh
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="${HERE%/*/*}"
SCRIPT="${REPO}/scripts/kb-verify.sh"
TMP_ROOT="$(mktemp -d)"
cleanup() {
  rm -f "${TMP_ROOT}"/case.*/calls.log
  rmdir "${TMP_ROOT}"/case.* "${TMP_ROOT}" 2>/dev/null || true
}
trap cleanup EXIT
FAILURES=0
TESTS=0

# run_case SCENARIO [script args...]: sets OUT, RC and FAKE_DIR. KB_ID comes from env unless
# KB_ID_UNDER_TEST is set to the empty string.
run_case() {
  local scenario="$1"
  shift
  FAKE_DIR="$(mktemp -d "${TMP_ROOT}/case.XXXXXX")"
  : >"${FAKE_DIR}/calls.log"
  OUT="$(PATH="${HERE}:${PATH}" FAKE_AWS_DIR="${FAKE_DIR}" FAKE_AWS_SCENARIO="${scenario}" \
    KB_ID="${KB_ID_UNDER_TEST-KBFAKE}" AWS_REGION="${AWS_REGION_UNDER_TEST:-eu-west-1}" \
    "${SCRIPT}" "$@" 2>&1)"
  RC=$?
}

check() { # check NAME CONDITION_RESULT(0=ok)
  TESTS=$((TESTS + 1))
  if [ "$2" -eq 0 ]; then
    echo "ok   - $1"
  else
    echo "FAIL - $1"
    echo "${OUT}" | sed 's/^/       | /'
    FAILURES=$((FAILURES + 1))
  fi
}

has() { grep -q -- "$1" <<<"${OUT}"; }
yes() { has "$1" && echo 0 || echo 1; }
no() { has "$1" && echo 1 || echo 0; }
retrieves() { grep 'bedrock-agent-runtime retrieve' "${FAKE_DIR}/calls.log"; }
# every_retrieve_has FIXED_STRING: 0 when every logged retrieve contains it.
every_retrieve_has() { retrieves | grep -vF -- "$1" | grep -q . && echo 1 || echo 0; }

test_happy_path() {
  run_case happy
  check "happy: exit 0" "$((RC != 0))"
  check "happy: no FAIL line" "$(no '^FAIL')"
  check "happy: summary counts" "$(yes 'SUMMARY: 7 PASS, 0 FAIL, 3 INFO')"
  check "happy: 1a reports later content" "$(yes 'INFO: 1a baseline: 1 of 7 results have version_min > 301')"
  check "happy: 1c runs with 302" "$(yes '1c X=302')"
  check "happy: string numbers handled (cloud-regions listed with version_min=0)" \
    "$(yes 'version_min=0 version_below=99999')"
  check "happy: cloud note lists cloud result" "$(yes 'INFO: 2a: 1 results with deployment=cloud (expected > 0): https://docs.formance.com/deploy/cloud-regions')"
  check "happy: ee note lists ee result" "$(yes 'INFO: 3a: 1 results with license=ee')"
  check "happy: output shows score and source_url" "$(yes 'score=0.91 product=payments .*version_below=99999')"
  check "happy: read-only calls only" "$(grep -vE 'bedrock-agent-runtime retrieve|get-knowledge-base' "${FAKE_DIR}/calls.log" | grep -q . && echo 1 || echo 0)"
}

test_filter_shape() {
  run_case happy --results 3 --stack-version 3.1
  check "calls: 8 retrieves" "$(retrieves | grep -c . | grep -qx 8 && echo 0 || echo 1)"
  check "calls: client_id in [none] in every retrieve" \
    "$(every_retrieve_has '{"in":{"key":"client_id","value":["none"]}}')"
  check "calls: baseline filter is the bare in condition (not wrapped)" \
    "$(retrieves | head -1 | grep -qF '"filter":{"in":{"key":"client_id","value":["none"]}}' && echo 0 || echo 1)"
  check "calls: every other retrieve uses a top-level andAll" \
    "$(retrieves | tail -n +2 | grep -vF '"filter":{"andAll":[{"in":{"key":"client_id"' | grep -q . && echo 1 || echo 0)"
  check "calls: numberOfResults honoured" "$(every_retrieve_has '"numberOfResults":3')"
  check "calls: region passed" "$(every_retrieve_has '--region eu-west-1')"
  check "calls: test 4 is the 5-condition filter with 301" \
    "$(retrieves | tail -1 | grep -qF '{"andAll":[{"in":{"key":"client_id","value":["none"]}},{"lessThanOrEquals":{"key":"version_min","value":301}},{"greaterThan":{"key":"version_below","value":301}},{"in":{"key":"deployment","value":["cloud","any"]}},{"in":{"key":"license","value":["ee","any"]}}]}' && echo 0 || echo 1)"
}

test_violation() {
  run_case leak
  check "leak: exit non-zero" "$((RC == 0))"
  check "leak: FAIL for numeric filter" "$(yes 'FAIL: 1b X=301: .* violate the filter')"
  check "leak: FAIL for deployment" "$(yes 'FAIL: 2a deployment in')"
  check "leak: FAIL for license" "$(yes 'FAIL: 3b license in')"
  check "leak: FAIL for combined" "$(yes 'FAIL: 4 combined')"
  check "leak: violation listed" "$(yes 'violation: https://docs.formance.com/tickets/acme-1')"
}

test_numeric_rejected() {
  run_case numeric_reject
  check "rejected: exit non-zero" "$((RC == 0))"
  check "rejected: banner" "$(yes 'NUMERIC FILTERS NOT SUPPORTED: use the fallback in docs/kb-contract-changes.md')"
  check "rejected: FAIL on 1b and 1c" "$(has 'FAIL: 1b X=301: numeric filter rejected' && has 'FAIL: 1c X=302: numeric filter rejected' && echo 0 || echo 1)"
  check "rejected: API error shown" "$(yes 'ValidationException')"
  check "rejected: tests 2 and 3 still run" "$(has 'PASS: 2a' && has 'PASS: 3b' && echo 0 || echo 1)"
}

test_numeric_other_error() {
  run_case numeric_other_error
  check "other error: exit 1" "$([ "${RC}" -eq 1 ] && echo 0 || echo 1)"
  check "other error: no banner" "$(no 'NUMERIC FILTERS NOT SUPPORTED')"
  check "other error: reported as retrieve failed" "$(yes 'FAIL: 1b X=301: retrieve failed: .*AccessDeniedException')"
}

test_numeric_empty() {
  run_case numeric_empty
  check "numeric_empty: FAIL, exit 1" "$([ "${RC}" -eq 1 ] && has 'FAIL: 1b/1c: numeric filter matched nothing although matching content exists; numbers may be stored as strings' && echo 0 || echo 1)"
  check "numeric_empty: no inconclusive INFO for 1b/1c" "$(no 'INFO: 1[bc] X=')"
}

test_client_id_leak() {
  run_case leak_client
  check "leak_client: exit 1" "$([ "${RC}" -eq 1 ] && echo 0 || echo 1)"
  check "leak_client: FAIL on 1b, 2a, 3a, 4" "$(has 'FAIL: 1b X=301' && has 'FAIL: 2a deployment' && has 'FAIL: 3a license' && has 'FAIL: 4 combined' && echo 0 || echo 1)"
  check "leak_client: violation shows client_id=acme" "$(yes 'violation: .*client_id=acme')"
}

test_inconclusive_baseline() {
  run_case no_later
  check "no_later: exit 0" "$((RC != 0))"
  check "no_later: INCONCLUSIVE with suggestion" "$(yes 'INCONCLUSIVE, try another query')"
}

test_retrieve_failure() {
  run_case retrieve_fail
  check "retrieve_fail: exit 1" "$((RC != 1))"
  check "retrieve_fail: FAIL with error" "$(yes 'FAIL: 2a deployment in \[cloud, any\]: retrieve call failed: .*ThrottlingException')"
}

test_overrides_and_preflight() {
  run_case happy --preflight --baseline-query "my baseline" --cloud-query "my cloud" --ee-query "my ee" --kb-id OTHERKB
  check "flags: preflight PASS" "$(yes 'PASS: preflight: knowledge base OTHERKB exists')"
  check "flags: --kb-id overrides env" "$(every_retrieve_has '--knowledge-base-id OTHERKB')"
  check "flags: custom queries used" \
    "$(retrieves | grep -qF 'my baseline' && retrieves | grep -qF 'my cloud' && retrieves | grep -qF 'my ee' && echo 0 || echo 1)"
  run_case kb_missing --preflight
  check "preflight: missing KB -> exit 1, no retrieve" "$([ "${RC}" -eq 1 ] && ! retrieves >/dev/null && echo 0 || echo 1)"
}

test_config_guards() {
  AWS_REGION_UNDER_TEST=us-east-1 run_case happy
  check "region: us-east-1 refused (exit 2)" "$([ "${RC}" -eq 2 ] && has "only eu-\* regions" && echo 0 || echo 1)"
  check "region: no aws call" "$([ ! -s "${FAKE_DIR}/calls.log" ] && echo 0 || echo 1)"
  run_case happy --region ap-south-1
  check "region: --region flag refused too" "$([ "${RC}" -eq 2 ] && echo 0 || echo 1)"
  run_case happy --region eu-central-1
  check "region: eu-central-1 accepted" "$([ "${RC}" -eq 0 ] && echo 0 || echo 1)"
  KB_ID_UNDER_TEST="" run_case happy
  check "kb id: required (exit 2)" "$([ "${RC}" -eq 2 ] && has 'KB_ID is required' && echo 0 || echo 1)"
  run_case happy --results 0
  check "results: 0 refused" "$([ "${RC}" -eq 2 ] && echo 0 || echo 1)"
  run_case happy --stack-version 3
  check "stack-version: '3' refused" "$([ "${RC}" -eq 2 ] && echo 0 || echo 1)"
  run_case happy --stack-version 3.100
  check "stack-version: minor 100 refused (exit 2)" "$([ "${RC}" -eq 2 ] && has 'minor must be <= 99' && echo 0 || echo 1)"
  run_case happy --stack-version 3.099
  check "stack-version: minor 099 accepted (leading zeros)" "$([ "${RC}" -eq 0 ] && echo 0 || echo 1)"
  run_case happy --bogus
  check "unknown option refused" "$([ "${RC}" -eq 2 ] && echo 0 || echo 1)"
}

test_edge_responses() {
  run_case empty
  check "empty: exit 0, nothing to check" "$((RC != 0))"
  check "empty: '(no results)' printed and inconclusive INFO" "$(has '(no results)' && has 'INFO: 1b X=301: 0 results, nothing to check' && echo 0 || echo 1)"
  run_case garbage
  check "garbage: exit 1 with parse FAIL, no jq crash" "$([ "${RC}" -eq 1 ] && has 'could not parse the Retrieve response' && echo 0 || echo 1)"
  run_case sparse
  check "sparse: missing metadata and missing version keys are violations (FAIL, exit 1)" "$([ "${RC}" -eq 1 ] && has 'FAIL: 1b X=301' && has 'FAIL: 4 combined' && echo 0 || echo 1)"
  check "sparse: missing values printed as ?" "$(yes 'version_min=? version_below=?')"
}

test_query_quoting() {
  local q='a"b\c $(touch pwned) `id` '"'"'q'"'"' ; {"x":1}'
  run_case happy --baseline-query "${q}" --cloud-query "${q}" --ee-query "${q}"
  check "quoting: exit 0 with hostile query" "$((RC != 0))"
  check "quoting: query is JSON-escaped in every retrieve" \
    "$(retrieves | grep -vF -- '"text":"a\"b\\c $(touch pwned) `id` '"'"'q'"'"' ; {\"x\":1}"' | grep -q . && echo 1 || echo 0)"
  check "quoting: nothing executed" "$([ ! -e pwned ] && echo 0 || echo 1)"
}

test_more_config_guards() {
  run_case happy --results 101
  check "results: 101 refused" "$([ "${RC}" -eq 2 ] && echo 0 || echo 1)"
  run_case happy --results abc
  check "results: non numeric refused" "$([ "${RC}" -eq 2 ] && echo 0 || echo 1)"
  run_case happy --kb-id
  check "flag without value refused" "$([ "${RC}" -eq 2 ] && has 'needs a value' && echo 0 || echo 1)"
  run_case happy --stack-version 03.01
  check "stack-version: leading zeros are decimal (301)" "$(yes 'Stack v03.01 = 301')"
  run_case happy --stack-version 3.1.2
  check "stack-version: 3 parts refused" "$([ "${RC}" -eq 2 ] && echo 0 || echo 1)"
  run_case happy --help
  check "help: exit 0, no aws call" "$([ "${RC}" -eq 0 ] && [ ! -s "${FAKE_DIR}/calls.log" ] && echo 0 || echo 1)"
}

test_happy_path
test_filter_shape
test_violation
test_numeric_rejected
test_numeric_other_error
test_numeric_empty
test_client_id_leak
test_inconclusive_baseline
test_retrieve_failure
test_overrides_and_preflight
test_config_guards
test_edge_responses
test_query_quoting
test_more_config_guards

echo "----"
echo "${TESTS} checks, ${FAILURES} failed"
[ "${FAILURES}" -eq 0 ]
