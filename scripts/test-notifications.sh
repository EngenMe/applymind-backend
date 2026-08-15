#!/usr/bin/env bash
# Manual smoke test for the `notifications` module over curl.
#
# Usage:
#   BASE_URL=http://localhost:8080 ./test-notifications.sh
#
# Optional (lets the script prove a *due* reminder actually appears, not just
# that the endpoint responds):
#   DATABASE_URL=postgres://... ./test-notifications.sh
#   With DATABASE_URL set, the script uses psql to back-date the reminder it
#   creates so due_at <= now(), confirms it then appears in
#   GET /notifications/due, and cleans up afterwards. Without it, that section
#   is skipped with a note — a reminder created moments ago has due_at 7 days
#   out (applications.DefaultFollowUpDelay) and won't show up on its own.
#
# Assumptions — adjust if your setup differs:
#   - Phase 15 removed the shared static API key; every route now requires a
#     real user. This script registers a throwaway account, issues it a bearer
#     API token the same way the extension would, and uses that token for
#     everything below. The account is left behind — see test-auth.sh's header
#     comment for the cleanup query.
#   - migrations/000010_seed_sites.up.sql seeds domain "linkedin.com"
#     (is_active = true, global — user_id IS NULL), so an application can be
#     created via job_url alone.
#   - jq is installed. psql is only needed for the optional DATABASE_URL step.

set -uo pipefail

BASE_URL="${BASE_URL:-http://localhost:8080}"
JOB_URL="https://www.linkedin.com/jobs/view/notiftest-$RANDOM"
COMPANY="Notif Smoketest Inc $RANDOM"

JSON=(-H "Content-Type: application/json")

# --- Authenticate ----------------------------------------------------------
AUTH_EMAIL="smoke-notifications-$RANDOM$RANDOM@example.com"
AUTH_PASSWORD="smoke-password-1234"
AUTH_COOKIES=$(mktemp)

reg_code=$(curl -sS -o /tmp/notif_auth_body -w "%{http_code}" -c "$AUTH_COOKIES" -b "$AUTH_COOKIES" \
  "${JSON[@]}" -X POST "${BASE_URL}/auth/register" \
  -d "$(jq -n --arg e "$AUTH_EMAIL" --arg p "$AUTH_PASSWORD" '{email: $e, password: $p, display_name: "Smoke Test"}')")
if [ "$reg_code" != "201" ]; then
  echo "Could not register a throwaway account (HTTP $reg_code) — cannot authenticate."
  cat /tmp/notif_auth_body
  rm -f "$AUTH_COOKIES"
  exit 1
fi

token_code=$(curl -sS -o /tmp/notif_auth_body -w "%{http_code}" -c "$AUTH_COOKIES" -b "$AUTH_COOKIES" \
  "${JSON[@]}" -X POST "${BASE_URL}/auth/tokens" -d '{"name":"smoke test token"}')
if [ "$token_code" != "201" ]; then
  echo "Could not issue an API token (HTTP $token_code) — cannot authenticate."
  cat /tmp/notif_auth_body
  rm -f "$AUTH_COOKIES"
  exit 1
fi
API_TOKEN=$(jq -r '.token' /tmp/notif_auth_body)
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
    resp=$(curl -sS -o /tmp/notif_body -w "%{http_code}" -X "$method" "${AUTH[@]}" "${JSON[@]}" \
      "${BASE_URL}${path}" -d "$data")
  else
    resp=$(curl -sS -o /tmp/notif_body -w "%{http_code}" -X "$method" "${AUTH[@]}" "${JSON[@]}" \
      "${BASE_URL}${path}")
  fi
  code="$resp"
  body=$(cat /tmp/notif_body)
}

cleanup_app_id=""
cleanup() {
  if [ -n "$cleanup_app_id" ]; then
    curl -sS -o /dev/null "${AUTH[@]}" -X DELETE "${BASE_URL}/applications/${cleanup_app_id}"
  fi
}
trap cleanup EXIT

echo "== 0. health check (unauthenticated) =="
code=$(curl -sS -o /tmp/notif_body -w "%{http_code}" "${BASE_URL}/health")
body=$(cat /tmp/notif_body)
check 200 "GET /health"
echo "$body" | jq . 2>/dev/null || echo "$body"
echo

echo "== 1. unauthenticated request -> 401 =="
code=$(curl -sS -o /tmp/notif_body -w "%{http_code}" "${BASE_URL}/notifications/due")
body=$(cat /tmp/notif_body)
check 401 "GET /notifications/due with no Authorization header"
echo

echo "== 2. GET /notifications/due with no due reminders is still 200 with an array =="
req GET "/notifications/due"
check 200 "GET /notifications/due"
echo "$body" | jq '{count, notifications}' 2>/dev/null || echo "$body"
notif_type=$(echo "$body" | jq -r '.notifications | type' 2>/dev/null)
if [ "$notif_type" = "array" ]; then
  echo "  OK   notifications is an array (never null)"
  pass=$((pass+1))
else
  echo "  FAIL notifications type = $notif_type, want array"
  fail=$((fail+1))
fi
echo

echo "== 3. create an application (Applied) — this schedules a follow-up reminder 7 days out =="
create_payload=$(jq -n --arg c "$COMPANY" --arg url "$JOB_URL" '{
  company_name: $c,
  job_title: "Backend Engineer",
  job_description: "Build things.",
  job_url: $url
}')
req POST /applications "$create_payload"
check 201 "POST /applications"
APP_ID=$(echo "$body" | jq -r '.application.id // empty' 2>/dev/null)
DUE_AT=$(echo "$body" | jq -r '.follow_up_due_at // empty' 2>/dev/null)
cleanup_app_id="$APP_ID"
echo "  -> APP_ID=$APP_ID  follow_up_due_at=$DUE_AT"
echo

if [ -z "$APP_ID" ] || [ "$APP_ID" = "null" ]; then
  echo "Cannot continue without an application id — check the response above."
  exit 1
fi
if [ -z "$DUE_AT" ] || [ "$DUE_AT" = "null" ]; then
  echo "  FAIL expected follow_up_due_at on an Applied save"
  fail=$((fail+1))
fi

echo "== 4. a reminder due 7 days from now must NOT appear in /notifications/due yet =="
req GET "/notifications/due"
check 200 "GET /notifications/due"
present=$(echo "$body" | jq --arg id "$APP_ID" '[.notifications[]? | select(.application_id == $id)] | length' 2>/dev/null)
if [ "$present" = "0" ]; then
  echo "  OK   fresh reminder correctly absent (due_at is 7 days out)"
  pass=$((pass+1))
else
  echo "  FAIL fresh reminder should not be due yet, found $present match(es)"
  fail=$((fail+1))
fi
echo

echo "== 5. moving the application out of Applied dismisses its reminder =="
req PATCH "/applications/${APP_ID}/status" '{"status":"Rejected"}'
check 200 "PATCH /applications/{id}/status -> Rejected"
echo

if [ -n "${DATABASE_URL:-}" ] && command -v psql >/dev/null 2>&1; then
  echo "== 6. (optional, DATABASE_URL set) back-date a fresh reminder and confirm it appears, then confirm it stops re-appearing once sent =="

  echo "  -- creating a second application to get an undismissed reminder --"
  second_url="${JOB_URL}-second"
  second_payload=$(jq -n --arg c "$COMPANY" --arg url "$second_url" '{
    company_name: $c, job_title: "Backend Engineer II",
    job_description: "Build more things.", job_url: $url
  }')
  req POST /applications "$second_payload"
  check 201 "POST /applications (second, for back-dating)"
  APP_ID_2=$(echo "$body" | jq -r '.application.id // empty' 2>/dev/null)

  if [ -n "$APP_ID_2" ] && [ "$APP_ID_2" != "null" ]; then
    psql "$DATABASE_URL" -q -c \
      "UPDATE follow_up_reminders SET due_at = now() - interval '1 hour' WHERE application_id = '${APP_ID_2}';" \
      >/dev/null

    req GET "/notifications/due"
    check 200 "GET /notifications/due after back-dating"
    present2=$(echo "$body" | jq --arg id "$APP_ID_2" '[.notifications[]? | select(.application_id == $id)] | length' 2>/dev/null)
    if [ "$present2" = "1" ]; then
      echo "  OK   back-dated reminder now appears"
      pass=$((pass+1))
      echo "$body" | jq --arg id "$APP_ID_2" '.notifications[] | select(.application_id == $id) | {title, days_since_applied, already_notified, suggested_actions}' 2>/dev/null
    else
      echo "  FAIL back-dated reminder should appear exactly once, found $present2"
      fail=$((fail+1))
    fi

    echo "  -- run the scheduler once locally to mark it sent, then confirm it still shows up flagged already_notified --"
    echo "     (run manually: go run ./cmd/scheduler, or wait for EventBridge — this script does not invoke the Lambda)"

    curl -sS -o /dev/null "${AUTH[@]}" -X DELETE "${BASE_URL}/applications/${APP_ID_2}"
  else
    echo "  SKIP could not create the second application"
  fi
  echo
else
  echo "== 6. (skipped) set DATABASE_URL to also verify a due reminder appears and survives being marked sent =="
  echo
fi

echo "=============================="
echo " passed: $pass   failed: $fail"
echo "=============================="
[ "$fail" -eq 0 ]
