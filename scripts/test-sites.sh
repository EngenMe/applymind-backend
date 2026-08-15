#!/usr/bin/env bash
# Manual smoke test for the `sites` module over curl.
#
# Usage:
#   BASE_URL=http://localhost:8080 ./test-sites.sh
#
# Assumptions — adjust if your setup differs:
#   - Phase 15 removed the shared static API key; every route now requires a
#     real user. This script registers a throwaway account, issues it a bearer
#     API token the same way the extension would, and uses that token for
#     everything below. The account is left behind — see test-auth.sh's header
#     comment for the cleanup query.
#   - cmd/api/main.go calls sites.Service.SeedPreconfigured at startup, so by
#     the time this script runs the eleven pre-configured (global, user_id
#     IS NULL) sites already exist, LinkedIn active and the rest inactive. If
#     you haven't wired that call in yet, steps 1-3 will still pass against
#     whatever migration 000010 alone seeded (LinkedIn only) — steps 4 onward
#     do not depend on the full list.
#   - jq is installed (`apt install jq` / `brew install jq`) for readable output.

set -uo pipefail

BASE_URL="${BASE_URL:-http://localhost:8080}"
CUSTOM_NAME="Smoketest Boards $RANDOM"
CUSTOM_DOMAIN="smoketest-$RANDOM.example.com"

JSON=(-H "Content-Type: application/json")

# --- Authenticate ----------------------------------------------------------
AUTH_EMAIL="smoke-sites-$RANDOM$RANDOM@example.com"
AUTH_PASSWORD="smoke-password-1234"
AUTH_COOKIES=$(mktemp)

reg_code=$(curl -sS -o /tmp/sites_auth_body -w "%{http_code}" -c "$AUTH_COOKIES" -b "$AUTH_COOKIES" \
  "${JSON[@]}" -X POST "${BASE_URL}/auth/register" \
  -d "$(jq -n --arg e "$AUTH_EMAIL" --arg p "$AUTH_PASSWORD" '{email: $e, password: $p, display_name: "Smoke Test"}')")
if [ "$reg_code" != "201" ]; then
  echo "Could not register a throwaway account (HTTP $reg_code) — cannot authenticate."
  cat /tmp/sites_auth_body
  rm -f "$AUTH_COOKIES"
  exit 1
fi

token_code=$(curl -sS -o /tmp/sites_auth_body -w "%{http_code}" -c "$AUTH_COOKIES" -b "$AUTH_COOKIES" \
  "${JSON[@]}" -X POST "${BASE_URL}/auth/tokens" -d '{"name":"smoke test token"}')
if [ "$token_code" != "201" ]; then
  echo "Could not issue an API token (HTTP $token_code) — cannot authenticate."
  cat /tmp/sites_auth_body
  rm -f "$AUTH_COOKIES"
  exit 1
fi
API_TOKEN=$(jq -r '.token' /tmp/sites_auth_body)
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
    resp=$(curl -sS -o /tmp/sites_body -w "%{http_code}" -X "$method" "${AUTH[@]}" "${JSON[@]}" \
      "${BASE_URL}${path}" -d "$data")
  else
    resp=$(curl -sS -o /tmp/sites_body -w "%{http_code}" -X "$method" "${AUTH[@]}" "${JSON[@]}" \
      "${BASE_URL}${path}")
  fi
  code="$resp"
  body=$(cat /tmp/sites_body)
}

echo "== 0. health check (unauthenticated) =="
code=$(curl -sS -o /tmp/sites_body -w "%{http_code}" "${BASE_URL}/health")
body=$(cat /tmp/sites_body)
check 200 "GET /health"
echo "$body" | jq . 2>/dev/null || echo "$body"
echo

echo "== 1. list all sites, expect LinkedIn pre-configured and active =="
req GET /sites
check 200 "GET /sites"
LINKEDIN_ACTIVE=$(echo "$body" | jq -r '.sites[] | select(.domain == "linkedin.com") | .is_active' 2>/dev/null)
LINKEDIN_PRECONF=$(echo "$body" | jq -r '.sites[] | select(.domain == "linkedin.com") | .is_preconfigured' 2>/dev/null)
if [ "$LINKEDIN_ACTIVE" = "true" ] && [ "$LINKEDIN_PRECONF" = "true" ]; then
  echo "  OK   linkedin.com is present, pre-configured and active"
  pass=$((pass+1))
else
  echo "  FAIL linkedin.com missing or not pre-configured/active (is_active=$LINKEDIN_ACTIVE, is_preconfigured=$LINKEDIN_PRECONF)"
  fail=$((fail+1))
fi
echo "$body" | jq '.sites | length' 2>/dev/null
echo

echo "== 2. list active-only sites, expect LinkedIn but not, e.g., Indeed =="
req GET "/sites?active=true"
check 200 "GET /sites?active=true"
ALL_ACTIVE=$(echo "$body" | jq -r '[.sites[].is_active] | all' 2>/dev/null)
if [ "$ALL_ACTIVE" = "true" ]; then
  echo "  OK   every returned site has is_active=true"
  pass=$((pass+1))
else
  echo "  FAIL an inactive site leaked into the active-only list"
  fail=$((fail+1))
fi
echo

echo "== 3. bad active filter value -> 400 =="
req GET "/sites?active=maybe"
check 400 "GET /sites?active=maybe"
echo "$body" | jq . 2>/dev/null || echo "$body"
echo

echo "== 4. add a custom site =="
add_payload=$(jq -n --arg n "$CUSTOM_NAME" --arg d "https://${CUSTOM_DOMAIN}/careers" '{name: $n, domain: $d}')
req POST /sites "$add_payload"
check 201 "POST /sites"
echo "$body" | jq . 2>/dev/null || echo "$body"
CUSTOM_ID=$(echo "$body" | jq -r '.id // empty' 2>/dev/null)
STORED_DOMAIN=$(echo "$body" | jq -r '.domain // empty' 2>/dev/null)
echo "  -> CUSTOM_ID=$CUSTOM_ID"

if [ -z "$CUSTOM_ID" ] || [ "$CUSTOM_ID" = "null" ]; then
  echo "Cannot continue without a site id — check the response above."
  exit 1
fi

if [ "$STORED_DOMAIN" = "$CUSTOM_DOMAIN" ]; then
  echo "  OK   domain normalised to bare host ($STORED_DOMAIN)"
  pass=$((pass+1))
else
  echo "  FAIL domain normalisation: got '$STORED_DOMAIN', expected '$CUSTOM_DOMAIN'"
  fail=$((fail+1))
fi

IS_PRECONF=$(echo "$body" | jq -r '.is_preconfigured' 2>/dev/null)
IS_ACTIVE=$(echo "$body" | jq -r '.is_active' 2>/dev/null)
if [ "$IS_PRECONF" = "false" ] && [ "$IS_ACTIVE" = "true" ]; then
  echo "  OK   custom site defaults: is_preconfigured=false, is_active=true"
  pass=$((pass+1))
else
  echo "  FAIL custom site defaults wrong (is_preconfigured=$IS_PRECONF, is_active=$IS_ACTIVE)"
  fail=$((fail+1))
fi
echo

echo "== 5. adding the same domain again -> 409 (unique constraint) =="
req POST /sites "$add_payload"
check 409 "POST /sites duplicate domain"
echo "$body" | jq . 2>/dev/null || echo "$body"
echo

echo "== 6. validation: missing name -> 400 =="
req POST /sites '{"domain":"example.com"}'
check 400 "POST /sites missing name"
echo

echo "== 7. validation: missing domain -> 400 =="
req POST /sites '{"name":"No Domain"}'
check 400 "POST /sites missing domain"
echo

echo "== 8. validation: unusable domain -> 400 =="
req POST /sites '{"name":"Bad Domain","domain":"not a domain"}'
check 400 "POST /sites invalid domain"
echo "$body" | jq . 2>/dev/null || echo "$body"
echo

echo "== 9. toggle the custom site off (defaults to active, so this deactivates) =="
req PATCH "/sites/${CUSTOM_ID}/toggle" ""
check 200 "PATCH /sites/{id}/toggle"
TOGGLED_ACTIVE=$(echo "$body" | jq -r '.is_active' 2>/dev/null)
if [ "$TOGGLED_ACTIVE" = "false" ]; then
  echo "  OK   is_active flipped to false"
  pass=$((pass+1))
else
  echo "  FAIL expected is_active=false after toggle, got $TOGGLED_ACTIVE"
  fail=$((fail+1))
fi
echo

echo "== 10. toggle it back on =="
req PATCH "/sites/${CUSTOM_ID}/toggle" ""
check 200 "PATCH /sites/{id}/toggle (again)"
TOGGLED_ACTIVE=$(echo "$body" | jq -r '.is_active' 2>/dev/null)
if [ "$TOGGLED_ACTIVE" = "true" ]; then
  echo "  OK   is_active flipped back to true"
  pass=$((pass+1))
else
  echo "  FAIL expected is_active=true after second toggle, got $TOGGLED_ACTIVE"
  fail=$((fail+1))
fi
echo

echo "== 11. toggle unknown id -> 404 =="
req PATCH "/sites/00000000-0000-0000-0000-000000000000/toggle" ""
check 404 "PATCH /sites/{unknown-id}/toggle"
echo

echo "== 12. toggle malformed uuid in path -> 400 =="
req PATCH "/sites/not-a-uuid/toggle" ""
check 400 "PATCH /sites/not-a-uuid/toggle"
echo

echo "== 13. deleting a pre-configured site (LinkedIn) -> 409, deactivate only =="
LINKEDIN_ID=$(curl -sS "${AUTH[@]}" "${BASE_URL}/sites" | jq -r '.sites[] | select(.domain == "linkedin.com") | .id' 2>/dev/null)
if [ -n "$LINKEDIN_ID" ] && [ "$LINKEDIN_ID" != "null" ]; then
  req DELETE "/sites/${LINKEDIN_ID}"
  check 409 "DELETE /sites/{linkedin-id}"
  echo "$body" | jq . 2>/dev/null || echo "$body"
else
  echo "  SKIP could not resolve linkedin.com's id — is seeding wired up?"
fi
echo

echo "== 14. delete unknown id -> 404 =="
req DELETE "/sites/00000000-0000-0000-0000-000000000000"
check 404 "DELETE unknown id"
echo

echo "== 15. delete malformed uuid in path -> 400 =="
req DELETE "/sites/not-a-uuid"
check 400 "DELETE /sites/not-a-uuid"
echo

echo "== 16. unauthenticated request -> 401 =="
code=$(curl -sS -o /tmp/sites_body -w "%{http_code}" "${BASE_URL}/sites")
body=$(cat /tmp/sites_body)
check 401 "GET /sites with no Authorization header"
echo

echo "== 17. delete the custom site (should succeed — it's not pre-configured and has no applications) =="
req DELETE "/sites/${CUSTOM_ID}"
check 204 "DELETE /sites/{custom-id}"
echo

echo "== 18. get it back via list -> should be gone =="
req GET /sites
check 200 "GET /sites after delete"
STILL_PRESENT=$(echo "$body" | jq -r --arg id "$CUSTOM_ID" '[.sites[].id] | index($id) != null' 2>/dev/null)
if [ "$STILL_PRESENT" = "false" ]; then
  echo "  OK   custom site no longer in the list"
  pass=$((pass+1))
else
  echo "  FAIL custom site still present after delete"
  fail=$((fail+1))
fi
echo

echo "== 19. deleting it again -> 404 (already gone) =="
req DELETE "/sites/${CUSTOM_ID}"
check 404 "DELETE /sites/{custom-id} (already deleted)"
echo

echo "=============================="
echo " passed: $pass   failed: $fail"
echo "=============================="
[ "$fail" -eq 0 ]
