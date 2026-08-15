#!/usr/bin/env bash
# Manual smoke test for the `applications` module over curl.
#
# Usage:
#   BASE_URL=http://localhost:8080 ./test-applications.sh
#
# Assumptions — adjust if your setup differs:
#   - Phase 15 removed the shared static API key; every route now requires a
#     real user. This script registers a throwaway account, issues it a bearer
#     API token the same way the extension would, and uses that token for
#     everything below. The account is left behind — see test-auth.sh's header
#     comment for the cleanup query.
#   - migrations/000010_seed_sites.up.sql seeds a row with domain "linkedin.com"
#     (is_active = true, global — user_id IS NULL) — that's what lets the
#     site_id-resolution-from-job_url path succeed below without ever calling a
#     /sites endpoint. If your seed uses a different domain, change JOB_URL.
#   - jq is installed (`apt install jq` / `brew install jq`) for readable output.

set -uo pipefail

BASE_URL="${BASE_URL:-http://localhost:8080}"
JOB_URL="https://www.linkedin.com/jobs/view/smoketest-$RANDOM"
COMPANY="Acme Corp $RANDOM"

JSON=(-H "Content-Type: application/json")

# --- Authenticate ----------------------------------------------------------
AUTH_EMAIL="smoke-applications-$RANDOM$RANDOM@example.com"
AUTH_PASSWORD="smoke-password-1234"
AUTH_COOKIES=$(mktemp)

reg_code=$(curl -sS -o /tmp/appl_auth_body -w "%{http_code}" -c "$AUTH_COOKIES" -b "$AUTH_COOKIES" \
  "${JSON[@]}" -X POST "${BASE_URL}/auth/register" \
  -d "$(jq -n --arg e "$AUTH_EMAIL" --arg p "$AUTH_PASSWORD" '{email: $e, password: $p, display_name: "Smoke Test"}')")
if [ "$reg_code" != "201" ]; then
  echo "Could not register a throwaway account (HTTP $reg_code) — cannot authenticate."
  cat /tmp/appl_auth_body
  rm -f "$AUTH_COOKIES"
  exit 1
fi

token_code=$(curl -sS -o /tmp/appl_auth_body -w "%{http_code}" -c "$AUTH_COOKIES" -b "$AUTH_COOKIES" \
  "${JSON[@]}" -X POST "${BASE_URL}/auth/tokens" -d '{"name":"smoke test token"}')
if [ "$token_code" != "201" ]; then
  echo "Could not issue an API token (HTTP $token_code) — cannot authenticate."
  cat /tmp/appl_auth_body
  rm -f "$AUTH_COOKIES"
  exit 1
fi
API_TOKEN=$(jq -r '.token' /tmp/appl_auth_body)
rm -f "$AUTH_COOKIES"

AUTH=(-H "Authorization: Bearer ${API_TOKEN}")

pass=0
fail=0

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

req() {
  local method="$1" path="$2" data="${3:-}"
  local resp
  if [ -n "$data" ]; then
    resp=$(curl -sS -o /tmp/appl_body -w "%{http_code}" -X "$method" "${AUTH[@]}" "${JSON[@]}" \
      "${BASE_URL}${path}" -d "$data")
  else
    resp=$(curl -sS -o /tmp/appl_body -w "%{http_code}" -X "$method" "${AUTH[@]}" "${JSON[@]}" \
      "${BASE_URL}${path}")
  fi
  code="$resp"
  body=$(cat /tmp/appl_body)
}

echo "== 0. health check (unauthenticated) =="
code=$(curl -sS -o /tmp/appl_body -w "%{http_code}" "${BASE_URL}/health")
body=$(cat /tmp/appl_body)
check 200 "GET /health"
echo "$body" | jq . 2>/dev/null || echo "$body"
echo

echo "== 1. create an application (defaults to Applied) =="
create_payload=$(jq -n --arg c "$COMPANY" --arg url "$JOB_URL" '{
  company_name: $c,
  job_title: "Backend Engineer",
  job_description: "Build things.",
  job_url: $url,
  cover_letter_text: "Dear hiring team, I would love to join Acme."
}')
req POST /applications "$create_payload"
check 201 "POST /applications"
echo "$body" | jq . 2>/dev/null || echo "$body"
APP_ID=$(echo "$body" | jq -r '.application.id // empty' 2>/dev/null)
echo "  -> APP_ID=$APP_ID"
echo

if [ -z "$APP_ID" ] || [ "$APP_ID" = "null" ]; then
  echo "Cannot continue without an application id — check the response above (site resolution is the likely failure point; see JOB_URL note at top of script)."
  exit 1
fi

echo "== 2. get it back, expect status_history with one entry (from_status: null) =="
req GET "/applications/${APP_ID}"
check 200 "GET /applications/{id}"
echo "$body" | jq '{status, applied_at, status_history}' 2>/dev/null || echo "$body"
echo

echo "== 3. cover letter should already exist (written during create) =="
req GET "/applications/${APP_ID}/coverletter"
check 200 "GET /applications/{id}/coverletter"
echo "$body" | jq . 2>/dev/null || echo "$body"
echo

echo "== 4. list applications, filtered by status=Applied =="
req GET "/applications?status=Applied&limit=5"
check 200 "GET /applications?status=Applied"
echo "$body" | jq '.applications | length' 2>/dev/null
echo

echo "== 5. duplicate check should now find this company =="
req GET "/applications/check-duplicate?company=$(python3 -c "import urllib.parse,sys;print(urllib.parse.quote(sys.argv[1]))" "$COMPANY" 2>/dev/null || echo "$COMPANY")"
check 200 "GET /applications/check-duplicate?company=..."
echo "$body" | jq . 2>/dev/null || echo "$body"
echo

echo "== 6. creating a second application, same company but a DIFFERENT posting/job_url, returns a duplicate warning but still 201s =="
# Deliberately a different job_url from step 1: reusing the same one would hit
# unique(site_id, job_url) instead (a different, harder check — see step 6b).
second_payload=$(jq -n --arg c "$COMPANY" --arg url "${JOB_URL}-second" '{
  company_name: $c,
  job_title: "Backend Engineer II",
  job_description: "Build more things.",
  job_url: $url
}')
req POST /applications "$second_payload"
check 201 "POST /applications (duplicate company, new posting)"
echo "$body" | jq '.duplicate' 2>/dev/null || echo "$body"
DUP_APP_ID=$(echo "$body" | jq -r '.application.id // empty' 2>/dev/null)
echo

echo "== 6b. re-posting the exact SAME job_url as step 1 should 409 (unique site_id+job_url, not the soft check) =="
req POST /applications "$create_payload"
check 409 "POST /applications (exact same job_url again)"
echo "$body" | jq . 2>/dev/null || echo "$body"
echo

echo "== 7. patch status Applied -> Acknowledged =="
req PATCH "/applications/${APP_ID}/status" '{"status":"Acknowledged"}'
check 200 "PATCH /applications/{id}/status"
echo "$body" | jq '{status, applied_at}' 2>/dev/null || echo "$body"
echo

echo "== 8. re-applying the same status should 409 (no-op transition) =="
req PATCH "/applications/${APP_ID}/status" '{"status":"Acknowledged"}'
check 409 "PATCH same status again"
echo "$body" | jq . 2>/dev/null || echo "$body"
echo

echo "== 9. history should now have 2 rows =="
req GET "/applications/${APP_ID}"
check 200 "GET /applications/{id} after status change"
echo "$body" | jq '.status_history | length' 2>/dev/null
echo

echo "== 10. put update (captured job data only, cannot move status) =="
update_payload=$(jq -n --arg c "$COMPANY" --arg url "$JOB_URL" '{
  company_name: $c,
  job_title: "Senior Backend Engineer",
  job_description: "Build bigger things.",
  job_url: $url
}')
req PUT "/applications/${APP_ID}" "$update_payload"
check 200 "PUT /applications/{id}"
echo "$body" | jq '{job_title, status}' 2>/dev/null || echo "$body"
echo

echo "== 11. validation: missing company_name -> 400 =="
req POST /applications '{"job_title":"x","job_url":"https://linkedin.com/x"}'
check 400 "POST /applications missing company_name"
echo

echo "== 12. unknown status value -> 400 =="
req PATCH "/applications/${APP_ID}/status" '{"status":"Not A Real Status"}'
check 400 "PATCH with invalid status"
echo

echo "== 13. malformed uuid in path -> 400 =="
req GET "/applications/not-a-uuid"
check 400 "GET /applications/not-a-uuid"
echo

echo "== 14. unauthenticated request -> 401 =="
code=$(curl -sS -o /tmp/appl_body -w "%{http_code}" "${BASE_URL}/applications")
body=$(cat /tmp/appl_body)
check 401 "GET /applications with no Authorization header"
echo

echo "== 15. delete the application =="
req DELETE "/applications/${APP_ID}"
check 204 "DELETE /applications/{id}"
echo

echo "== 16. get after delete -> 404 =="
req GET "/applications/${APP_ID}"
check 404 "GET /applications/{id} after delete"
echo "$body" | jq . 2>/dev/null || echo "$body"
echo

echo "== 17. delete unknown id -> 404 =="
req DELETE "/applications/00000000-0000-0000-0000-000000000000"
check 404 "DELETE unknown id"
echo

# cleanup the duplicate we created in step 6, if it exists
if [ -n "${DUP_APP_ID:-}" ] && [ "$DUP_APP_ID" != "null" ]; then
  curl -sS -o /dev/null "${AUTH[@]}" -X DELETE "${BASE_URL}/applications/${DUP_APP_ID}"
fi

echo "=============================="
echo " passed: $pass   failed: $fail"
echo "=============================="
[ "$fail" -eq 0 ]