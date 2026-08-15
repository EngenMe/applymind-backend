#!/usr/bin/env bash
# Manual smoke test for the `auth` module over curl.
#
# Usage:
#   BASE_URL=http://localhost:8080 ./test-auth.sh
#
# Assumptions — adjust if your setup differs:
#   - Phase 15 wiring: /auth/register and /auth/login are public; everything
#     else, including the sites/applications/cvs/coverletters/notifications
#     module group, is behind RequireAuth. The shared static API key from
#     phase 14 (LegacyStaticKeyAuth) is gone — step 24 checks that a bearer
#     token now works where the old key used to, and that garbage in the
#     Authorization header is rejected.
#   - APPLYMIND_COOKIE_SECURE=false in your local .env. Over plain http a
#     Secure cookie is stored by curl and then never sent back, so every
#     cookie-authenticated step would fail for a reason that has nothing to do
#     with the code. Step 2 detects this and says so rather than letting you
#     debug fifteen red lines.
#   - jq is installed (`apt install jq` / `brew install jq`) for readable output.
#
# This registers one throwaway account per run (smoke-$RANDOM@example.com) and
# leaves it behind: there is no delete-account endpoint in this phase. Clean up
# with `DELETE FROM users WHERE email LIKE 'smoke-%@example.com';` when the
# local database gets noisy.
#
# Not covered here, deliberately: the 30-day sliding expiry (a smoke test
# cannot wait 24 hours — internal/auth/service_test.go covers it with a fake
# clock) and login timing parity (measurable over a network, not over
# localhost).

set -uo pipefail

BASE_URL="${BASE_URL:-http://localhost:8080}"

EMAIL="smoke-$RANDOM$RANDOM@example.com"
PASSWORD="smoke-password-1234"
DISPLAY_NAME="Smoke Test"

BODY_FILE=/tmp/auth_body
HEADER_FILE=/tmp/auth_headers
JAR=/tmp/auth_cookies_1     # the browser that registers
JAR2=/tmp/auth_cookies_2    # a second browser, for the session-revocation steps
rm -f "$JAR" "$JAR2"

JSON=(-H "Content-Type: application/json")
AUTH_ARGS=()

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

# assert CONDITION_RESULT DESCRIPTION -- pass "true"/anything else
assert() {
  local ok="$1" desc="$2"
  if [ "$ok" = "true" ]; then
    echo "  OK   $desc"
    pass=$((pass+1))
  else
    echo "  FAIL $desc"
    fail=$((fail+1))
  fi
}

req() {
  local method="$1" path="$2" data="${3:-}"
  # --max-time is a safety net, not the fix: without it, any future request
  # that hangs for a reason we haven't seen yet blocks the script forever
  # instead of failing loudly with a curl exit code.
  local args=(-sS -o "$BODY_FILE" -D "$HEADER_FILE" -w "%{http_code}" --max-time 10 "${JSON[@]}")
  if [ "$method" = "HEAD" ]; then
    # curl -X HEAD is not curl -I. With -X HEAD curl still runs a normal
    # transfer and waits to read a body of Content-Length bytes; a spec-correct
    # HEAD response sends that header with zero actual body bytes, so curl
    # blocks forever waiting for bytes that are never coming. -I (--head) is
    # what actually tells curl not to expect one.
    args+=(-I)
  else
    args+=(-X "$method")
  fi
  args+=(${AUTH_ARGS[@]+"${AUTH_ARGS[@]}"})
  [ -n "$data" ] && args+=(-d "$data")
  code=$(curl "${args[@]}" "${BASE_URL}${path}")
  body=$(cat "$BODY_FILE")
}

# Credential selectors — set before each req.
as_none()    { AUTH_ARGS=(); }
as_cookie()  { AUTH_ARGS=(-b "$JAR" -c "$JAR"); }
as_cookie2() { AUTH_ARGS=(-b "$JAR2" -c "$JAR2"); }
as_bearer()  { AUTH_ARGS=(-H "Authorization: Bearer $1"); }
as_garbage() { AUTH_ARGS=(-H "Authorization: Bearer this-is-not-a-real-credential"); }

echo "== 0. health check (unauthenticated) =="
as_none
req GET /health
check 200 "GET /health"
echo

echo "== 1. register a new account -> 201 =="
as_cookie
reg_payload=$(jq -n --arg e "$EMAIL" --arg p "$PASSWORD" --arg d "$DISPLAY_NAME" \
  '{email: $e, password: $p, display_name: $d}')
req POST /auth/register "$reg_payload"
check 201 "POST /auth/register"
echo "$body" | jq . 2>/dev/null || echo "$body"
USER_ID=$(echo "$body" | jq -r '.user.id // empty' 2>/dev/null)
if [ -z "$USER_ID" ]; then
  echo "Cannot continue without a user id — check the response above."
  exit 1
fi
assert "$(echo "$body" | jq -r --arg e "$EMAIL" '.user.email == ($e | ascii_downcase)')" \
  "email stored normalised"
assert "$(echo "$body" | jq -r '.user.email_verified_at != null')" \
  "email_verified_at is set at registration"
assert "$(echo "$body" | jq -r 'tostring | test("password") | not')" \
  "response carries no password field of any kind"
echo

echo "== 2. session cookie attributes =="
SET_COOKIE=$(grep -i '^set-cookie:' "$HEADER_FILE" | tr -d '\r')
echo "  $SET_COOKIE"
for attr in "applymind_session" "HttpOnly" "SameSite=Lax" "Path=/"; do
  case "$SET_COOKIE" in
    *"$attr"*) assert true  "cookie has $attr" ;;
    *)         assert false "cookie has $attr" ;;
  esac
done
case "$BASE_URL:$SET_COOKIE" in
  http://*Secure*)
    echo
    echo "  !! The cookie is marked Secure and BASE_URL is plain http."
    echo "  !! curl will store it and never send it back, so every"
    echo "  !! cookie-authenticated step below will fail with 401."
    echo "  !! Set APPLYMIND_COOKIE_SECURE=false in .env and restart the API."
    echo
    ;;
esac
echo

echo "== 3. GET /auth/me with the cookie -> 200 =="
as_cookie
req GET /auth/me
check 200 "GET /auth/me (cookie)"
assert "$(echo "$body" | jq -r --arg id "$USER_ID" '.user.id == $id')" "returns the registered user"
echo

echo "== 4. GET /auth/me with no credential -> 401 =="
as_none
req GET /auth/me
check 401 "GET /auth/me (no credential)"
assert "$(echo "$body" | jq -r '.error == "unauthorized"')" "middleware envelope is {\"error\":\"unauthorized\"}"
echo

echo "== 5. registering the same email again -> 409, case-insensitively =="
as_none
upper_payload=$(jq -n --arg e "  $(echo "$EMAIL" | tr '[:lower:]' '[:upper:]')  " --arg p "$PASSWORD" \
  '{email: $e, password: $p}')
req POST /auth/register "$upper_payload"
check 409 "POST /auth/register (same address, uppercased and padded)"
assert "$(echo "$body" | jq -r '.error.code == "email_taken"')" "code is email_taken"
echo

echo "== 6. password below the 12-character minimum -> 400 =="
req POST /auth/register "$(jq -n --arg e "short-$EMAIL" '{email: $e, password: "01234567890"}')"
check 400 "POST /auth/register (11-character password)"
assert "$(echo "$body" | jq -r '.error.code == "password_too_short"')" "code is password_too_short"
echo

echo "== 7. malformed email -> 400 =="
req POST /auth/register "$(jq -n --arg p "$PASSWORD" '{email: "not an address", password: $p}')"
check 400 "POST /auth/register (malformed email)"
assert "$(echo "$body" | jq -r '.error.code == "email_invalid"')" "code is email_invalid"
echo

echo "== 8. wrong password and unknown email are indistinguishable =="
as_none
req POST /auth/login "$(jq -n --arg e "$EMAIL" '{email: $e, password: "wrong-password-here"}')"
check 401 "POST /auth/login (wrong password)"
wrong_pw_code="$code"; wrong_pw_body="$body"
req POST /auth/login "$(jq -n --arg p "$PASSWORD" '{email: "nobody-here-9999@example.com", password: $p}')"
check 401 "POST /auth/login (unknown email)"
if [ "$code" = "$wrong_pw_code" ] && [ "$body" = "$wrong_pw_body" ]; then
  assert true "both failures return an identical status and body"
else
  assert false "both failures return an identical status and body"
  echo "        wrong password: [$wrong_pw_code] $wrong_pw_body"
  echo "        unknown email:  [$code] $body"
fi
echo

echo "== 9. login with the right password, in a second browser -> 200 =="
as_cookie2
req POST /auth/login "$(jq -n --arg e "$EMAIL" --arg p "$PASSWORD" '{email: $e, password: $p}')"
check 200 "POST /auth/login"
assert "$(echo "$body" | jq -r --arg id "$USER_ID" '.user.id == $id')" "same account as registration"
as_cookie2
req GET /auth/me
check 200 "GET /auth/me (second browser's cookie)"
echo

echo "== 10. GET /auth/sessions -> 200, two sessions, exactly one marked current =="
as_cookie
req GET /auth/sessions
check 200 "GET /auth/sessions"
echo "$body" | jq '.sessions | length' 2>/dev/null
assert "$(echo "$body" | jq -r '[.sessions[] | select(.current)] | length == 1')" \
  "exactly one session is marked current"
assert "$(echo "$body" | jq -r '.sessions | length >= 2')" \
  "the second browser's session is listed too"
OTHER_SESSION_ID=$(echo "$body" | jq -r 'first(.sessions[] | select(.current | not) | .id) // empty' 2>/dev/null)
echo "  -> OTHER_SESSION_ID=$OTHER_SESSION_ID"
echo

echo "== 11. issue a full-access API token -> 201 =="
as_cookie
req POST /auth/tokens '{"name":"smoke full"}'
check 201 "POST /auth/tokens"
FULL_TOKEN=$(echo "$body" | jq -r '.token // empty' 2>/dev/null)
FULL_TOKEN_ID=$(echo "$body" | jq -r '.api_token.id // empty' 2>/dev/null)
assert "$([ -n "$FULL_TOKEN" ] && echo true || echo false)" "raw token is returned"
assert "$(echo "$body" | jq -r '.warning != "" and .warning != null')" "response carries the shown-once warning"
assert "$(echo "$body" | jq -r '.api_token.read_only == false')" "read_only defaults to false"
echo

echo "== 12. GET /auth/tokens -> 200, and never the hash =="
req GET /auth/tokens
check 200 "GET /auth/tokens"
assert "$(echo "$body" | jq -r --arg id "$FULL_TOKEN_ID" '[.api_tokens[].id] | index($id) != null')" \
  "the new token is listed"
assert "$(echo "$body" | jq -r 'tostring | test("token_hash|\"token\"") | not')" \
  "list exposes neither token_hash nor a raw token"
echo

echo "== 13. authenticate with the bearer token -> 200 =="
as_bearer "$FULL_TOKEN"
req GET /auth/me
check 200 "GET /auth/me (bearer)"
assert "$(echo "$body" | jq -r --arg id "$USER_ID" '.user.id == $id')" "resolves to the same user as the cookie"
echo

echo "== 14. issue a read-only token -> 201 =="
as_cookie
req POST /auth/tokens '{"name":"smoke read-only","read_only":true}'
check 201 "POST /auth/tokens (read_only:true)"
RO_TOKEN=$(echo "$body" | jq -r '.token // empty' 2>/dev/null)
RO_TOKEN_ID=$(echo "$body" | jq -r '.api_token.id // empty' 2>/dev/null)
assert "$(echo "$body" | jq -r '.api_token.read_only == true')" "read_only is reported back as true"
echo

echo "== 15. read-only token: safe methods pass =="
as_bearer "$RO_TOKEN"
req GET /auth/me
check 200 "GET /auth/me (read-only token)"

# HEAD is on the allow-list — the middleware must not treat it as a write —
# but no route in this API implements HEAD anywhere, on any endpoint, for any
# credential. chi does not derive a HEAD handler from a GET one. So the
# correct response here is chi's ordinary 405 for an unimplemented method,
# exactly what a full-access credential gets on the same request; a 403 would
# be the actual bug, since it would mean the safe-method allow-list itself is
# wrong.
as_bearer "$RO_TOKEN"
req HEAD /auth/tokens
check 405 "HEAD /auth/tokens (read-only token — not implemented, not blocked)"
echo

echo "== 16. read-only token: every write method -> 403 read_only_token =="
for method in POST PUT PATCH DELETE; do
  as_bearer "$RO_TOKEN"
  case "$method" in
    POST) req POST /auth/tokens '{"name":"should not exist"}' ;;
    *)    req "$method" "/auth/tokens/${FULL_TOKEN_ID}" ;;
  esac
  check 403 "$method /auth/tokens (read-only token)"
  assert "$(echo "$body" | jq -r '.error == "read_only_token"')" "  $method body is {\"error\":\"read_only_token\"}"
done
echo "  (403 and not 401: the credential is valid, so re-authenticating would change nothing)"
echo

echo "== 17. the write the read-only token attempted did not happen =="
as_cookie
req GET /auth/tokens
check 200 "GET /auth/tokens"
assert "$(echo "$body" | jq -r --arg id "$FULL_TOKEN_ID" '[.api_tokens[].id] | index($id) != null')" \
  "the full token the read-only DELETE targeted still exists"
assert "$(echo "$body" | jq -r '[.api_tokens[] | select(.name == "should not exist")] | length == 0')" \
  "the token the read-only POST tried to create was not created"
echo

echo "== 18. read-only is a property of the token, not the user =="
as_bearer "$FULL_TOKEN"
req POST /auth/tokens '{"name":"smoke third"}'
check 201 "POST /auth/tokens (full token, same account)"
THIRD_TOKEN_ID=$(echo "$body" | jq -r '.api_token.id // empty' 2>/dev/null)
echo

echo "== 19. revoke the read-only token -> 204, then it stops working =="
as_cookie
req DELETE "/auth/tokens/${RO_TOKEN_ID}"
check 204 "DELETE /auth/tokens/{id}"
as_bearer "$RO_TOKEN"
req GET /auth/me
check 401 "GET /auth/me (revoked token)"
echo

echo "== 20. token id validation =="
as_cookie
req DELETE "/auth/tokens/not-a-uuid"
check 400 "DELETE /auth/tokens/not-a-uuid"
as_cookie
req DELETE "/auth/tokens/00000000-0000-0000-0000-000000000000"
check 404 "DELETE /auth/tokens/{unknown}"
echo

echo "== 21. revoke the other browser's session -> 204, then its cookie stops working =="
if [ -n "$OTHER_SESSION_ID" ]; then
  as_cookie
  req DELETE "/auth/sessions/${OTHER_SESSION_ID}"
  check 204 "DELETE /auth/sessions/{id}"
  as_cookie2
  req GET /auth/me
  check 401 "GET /auth/me (revoked session)"
else
  echo "  SKIP no second session id resolved in step 10"
fi
as_cookie
req DELETE "/auth/sessions/00000000-0000-0000-0000-000000000000"
check 404 "DELETE /auth/sessions/{unknown}"
echo

echo "== 22. logout -> 204, and the cookie stops working =="
as_cookie
req POST /auth/logout
check 204 "POST /auth/logout"
as_cookie
req GET /auth/me
check 401 "GET /auth/me (after logout)"
echo

echo "== 23. logging out again is still fine =="
as_bearer "$FULL_TOKEN"
req POST /auth/logout
check 204 "POST /auth/logout (no session behind this credential)"
echo

echo "== 24. phase 15: the module group requires a real credential, not the removed static key =="
as_garbage
req GET /sites
check 401 "GET /sites (garbage bearer value — the old static key means nothing now)"
as_bearer "$FULL_TOKEN"
req GET /sites
check 200 "GET /sites (real auth token)"
echo "  (if the garbage credential returns 200, something is accepting requests"
echo "   without actually validating them; if the real token returns 401, the"
echo "   module group is still on the old static key)"
echo

echo "== 25. no credential on a protected route -> 401 =="
as_none
req POST /auth/tokens '{"name":"nope"}'
check 401 "POST /auth/tokens (no credential)"
echo

echo "=============================="
echo " passed: $pass   failed: $fail"
echo " account left behind: $EMAIL"
echo "=============================="
[ "$fail" -eq 0 ]