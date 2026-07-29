#!/usr/bin/env bash
# Manual smoke test for phase 7: the `settings` module and AI job scoring on
# `applications`.
#
# Usage:
#   BASE_URL=http://localhost:8080 API_KEY=devkey123 ./test-ai-scoring.sh
#
# Assumptions — adjust if your setup differs:
#   - Auth header is `Authorization: Bearer <API_KEY>` (same as
#     test-applications.sh, matching Flow 1 step 10's annotation).
#   - migrations/000010_seed_sites.up.sql seeds a row with domain
#     "linkedin.com" (is_active = true), used the same way
#     test-applications.sh uses it to resolve site_id from job_url.
#   - jq is installed.
#
# What this script CANNOT assume, and handles instead:
#   - Whether OPENAI_API_KEY is configured on the server. Scoring is optional
#     and fail-soft by design (see 07-backend-ai-job-scoring.md), so a NULL
#     ai_score is not a failure — sections 5-7 report what they see with `warn`
#     rather than asserting a score exists. If OPENAI_API_KEY is set and valid,
#     you should see a real score in section 5; if you don't, that's worth
#     investigating even though this script won't fail on it.
#
# Side effect worth knowing about: this script calls
# PUT /settings/profile-summary, which overwrites the single settings row.
# Section 1 reads whatever is there beforehand and restores it at the end —
# but if the script is killed mid-run, your profile summary will be left as
# the test value below. Nothing else here is destructive beyond the
# applications it creates and cleans up.

set -uo pipefail

BASE_URL="${BASE_URL:-http://localhost:8080}"
API_KEY="${API_KEY:-devkey}"
JOB_URL="https://www.linkedin.com/jobs/view/aismoketest-$RANDOM"
COMPANY="AI Smoke Test Corp $RANDOM"
TEST_SUMMARY="Backend engineer with six years of Go, PostgreSQL and AWS Lambda experience, focused on API design and distributed systems."

AUTH=(-H "Authorization: Bearer ${API_KEY}")
JSON=(-H "Content-Type: application/json")

pass=0
fail=0
warn=0

# check STATUS_EXPECTED DESCRIPTION -- reads $body and $code set by the caller
check() {
  local expected="$1" desc="$2"
  if [ "$code" = "$expected" ]; then
    echo "  OK   [$code] $desc"
    pass=$((pass+1))
  else
    echo "  FAIL [$code, expected $expected] $desc"
    echo "        $body" | head -c 400
    echo
    fail=$((fail+1))
  fi
}

# warn CONDITION DESCRIPTION -- for outcomes that depend on server config
# (OPENAI_API_KEY) rather than on this module's own correctness. Never fails
# the run; just flags it for a human to look at.
warn() {
  local condition="$1" desc="$2"
  if [ "$condition" = "true" ]; then
    echo "  OK   $desc"
    pass=$((pass+1))
  else
    echo "  WARN $desc"
    warn=$((warn+1))
  fi
}

req() {
  local method="$1" path="$2" data="${3:-}"
  local resp
  if [ -n "$data" ]; then
    resp=$(curl -sS -o /tmp/ai_body -w "%{http_code}" -X "$method" "${AUTH[@]}" "${JSON[@]}" \
      "${BASE_URL}${path}" -d "$data")
  else
    resp=$(curl -sS -o /tmp/ai_body -w "%{http_code}" -X "$method" "${AUTH[@]}" "${JSON[@]}" \
      "${BASE_URL}${path}")
  fi
  code="$resp"
  body=$(cat /tmp/ai_body)
}

echo "== 0. health check (unauthenticated) =="
code=$(curl -sS -o /tmp/ai_body -w "%{http_code}" "${BASE_URL}/health")
body=$(cat /tmp/ai_body)
check 200 "GET /health"
echo "$body" | jq . 2>/dev/null || echo "$body"
echo

# ---------------------------------------------------------------------------
# Settings: profile summary CRUD
# ---------------------------------------------------------------------------

echo "== 1. read whatever profile summary already exists, so it can be restored later =="
req GET /settings/profile-summary
check 200 "GET /settings/profile-summary"
echo "$body" | jq . 2>/dev/null || echo "$body"
ORIGINAL_SUMMARY=$(echo "$body" | jq -r '.profile_summary // empty' 2>/dev/null)
echo "  -> had previously: ${ORIGINAL_SUMMARY:-<none set>}"
echo

echo "== 2. set a profile summary =="
req PUT /settings/profile-summary "$(jq -n --arg s "$TEST_SUMMARY" '{profile_summary: $s}')"
check 200 "PUT /settings/profile-summary"
echo "$body" | jq . 2>/dev/null || echo "$body"
STORED_SUMMARY=$(echo "$body" | jq -r '.profile_summary // empty' 2>/dev/null)
if [ "$STORED_SUMMARY" = "$TEST_SUMMARY" ]; then
  echo "  OK   stored summary matches what was sent"
  pass=$((pass+1))
else
  echo "  FAIL stored summary = '$STORED_SUMMARY', want '$TEST_SUMMARY'"
  fail=$((fail+1))
fi
echo

echo "== 3. read it back, should match what was just set =="
req GET /settings/profile-summary
check 200 "GET /settings/profile-summary after write"
echo "$body" | jq . 2>/dev/null || echo "$body"
echo

echo "== 4. validation: empty, whitespace-only, and over-length summaries -> 400 =="
req PUT /settings/profile-summary '{"profile_summary": ""}'
check 400 "PUT with empty profile_summary"

req PUT /settings/profile-summary '{"profile_summary": "   "}'
check 400 "PUT with whitespace-only profile_summary"

TOO_LONG=$(python3 -c "print('a' * 2001)" 2>/dev/null || printf 'a%.0s' {1..2001})
req PUT /settings/profile-summary "$(jq -n --arg s "$TOO_LONG" '{profile_summary: $s}')"
check 400 "PUT with a 2001-character profile_summary"
echo

echo "== 4b. malformed body -> 400 =="
req PUT /settings/profile-summary 'not json'
check 400 "PUT /settings/profile-summary with a non-JSON body"
echo

# ---------------------------------------------------------------------------
# AI job scoring on application create
#
# With a profile summary now set (step 2), a job description on create should
# get scored -- if OPENAI_API_KEY is configured on the server. Whether it is
# configured is outside this script's control, so section 5 uses `warn`
# rather than `check` for the score fields themselves.
# ---------------------------------------------------------------------------

echo "== 5. create an application with a job description; check for an ai score =="
create_payload=$(jq -n --arg c "$COMPANY" --arg url "$JOB_URL" '{
  company_name: $c,
  job_title: "Senior Backend Engineer",
  job_description: "We are looking for a backend engineer with strong Go and PostgreSQL experience to build and scale our API platform on AWS.",
  job_url: $url
}')
req POST /applications "$create_payload"
check 201 "POST /applications"
echo "$body" | jq '{application: {id: .application.id, ai_score, ai_score_explanation}: .application | {id, ai_score, ai_score_explanation}}' 2>/dev/null \
  || echo "$body" | jq '.application | {id, ai_score, ai_score_explanation}' 2>/dev/null \
  || echo "$body"
APP_ID=$(echo "$body" | jq -r '.application.id // empty' 2>/dev/null)
AI_SCORE=$(echo "$body" | jq -r '.application.ai_score // empty' 2>/dev/null)
AI_EXPLANATION=$(echo "$body" | jq -r '.application.ai_score_explanation // empty' 2>/dev/null)
echo "  -> APP_ID=$APP_ID"

if [ -z "$APP_ID" ] || [ "$APP_ID" = "null" ]; then
  echo "Cannot continue without an application id — check the response above."
  exit 1
fi

if [ -n "$AI_SCORE" ] && [ "$AI_SCORE" != "null" ]; then
  warn "true" "ai_score = $AI_SCORE (OPENAI_API_KEY appears to be configured and scoring succeeded)"
  if [ -n "$AI_EXPLANATION" ] && [ "$AI_EXPLANATION" != "null" ]; then
    warn "true" "ai_score_explanation is present: \"$AI_EXPLANATION\""
  fi
else
  warn "false" "ai_score is null — expected if OPENAI_API_KEY is unset on the server; a problem if it IS set (check server logs for 'ai scoring failed')"
fi
echo

echo "== 6. get the application back; ai_score/explanation should match what create returned (never re-scored on read) =="
req GET "/applications/${APP_ID}"
check 200 "GET /applications/{id}"
REREAD_SCORE=$(echo "$body" | jq -r '.ai_score // empty' 2>/dev/null)
if [ "$REREAD_SCORE" = "$AI_SCORE" ]; then
  echo "  OK   ai_score on GET matches ai_score from POST ($REREAD_SCORE)"
  pass=$((pass+1))
else
  echo "  FAIL ai_score on GET = '$REREAD_SCORE', want it to match the create response '$AI_SCORE'"
  fail=$((fail+1))
fi
echo

echo "== 7. an application with NO job description must save with ai_score left null, regardless of OPENAI_API_KEY =="
no_jd_payload=$(jq -n --arg c "$COMPANY" --arg url "${JOB_URL}-no-jd" '{
  company_name: $c,
  job_title: "Backend Engineer (no JD)",
  job_description: "",
  job_url: $url
}')
req POST /applications "$no_jd_payload"
check 201 "POST /applications with an empty job_description"
NO_JD_APP_ID=$(echo "$body" | jq -r '.application.id // empty' 2>/dev/null)
NO_JD_SCORE=$(echo "$body" | jq -r '.application.ai_score // empty' 2>/dev/null)
if [ -z "$NO_JD_SCORE" ] || [ "$NO_JD_SCORE" = "null" ]; then
  echo "  OK   ai_score is null with no job description to score against"
  pass=$((pass+1))
else
  echo "  FAIL ai_score = '$NO_JD_SCORE', want null — there was nothing to score"
  fail=$((fail+1))
fi
echo

echo "== 8. the save itself must never fail because of scoring — confirm normal fields are intact on both applications above =="
req GET "/applications/${APP_ID}"
STATUS_1=$(echo "$body" | jq -r '.status // empty' 2>/dev/null)
req GET "/applications/${NO_JD_APP_ID}"
STATUS_2=$(echo "$body" | jq -r '.status // empty' 2>/dev/null)
if [ "$STATUS_1" = "Applied" ] && [ "$STATUS_2" = "Applied" ]; then
  echo "  OK   both applications saved and reached status Applied, independent of scoring outcome"
  pass=$((pass+1))
else
  echo "  FAIL status_1='$STATUS_1' status_2='$STATUS_2', want both 'Applied'"
  fail=$((fail+1))
fi
echo

# ---------------------------------------------------------------------------
# Cleanup
# ---------------------------------------------------------------------------

echo "== 9. cleanup: delete the applications created above =="
req DELETE "/applications/${APP_ID}"
check 204 "DELETE /applications/{id}"
req DELETE "/applications/${NO_JD_APP_ID}"
check 204 "DELETE /applications/{no-jd-id}"
echo

echo "== 10. restore the original profile summary (or leave the test one if none existed before) =="
if [ -n "$ORIGINAL_SUMMARY" ] && [ "$ORIGINAL_SUMMARY" != "null" ]; then
  req PUT /settings/profile-summary "$(jq -n --arg s "$ORIGINAL_SUMMARY" '{profile_summary: $s}')"
  check 200 "PUT /settings/profile-summary (restore original)"
else
  echo "  (no original summary existed — leaving the test value in place; nothing to restore)"
fi
echo

echo "=============================="
echo " passed: $pass   failed: $fail   warned: $warn"
echo "=============================="
[ "$fail" -eq 0 ]