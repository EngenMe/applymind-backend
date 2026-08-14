# Phase 8 — Deploy Checklist

## Fully scripted (CDK / Makefile)
- [ ] S3 bucket (`applymind-files-{account}-{region}`), versioned, public access blocked, CORS rules
- [ ] Lambda 1 `applymind-api` (arm64, provided.al2023, X-Ray active tracing)
- [ ] Lambda 2 `applymind-scheduler` (same runtime)
- [ ] IAM roles for both, scoped separately (S3 grant only on Lambda 1)
- [ ] CloudWatch log groups, 30-day retention, for both functions
- [ ] API Gateway REST API, proxy integration, regional endpoint, X-Ray on the stage
- [ ] ACM certificate for `api.applymind.faroukhasnaoui.tech`, DNS-validated automatically
- [ ] Route 53 A-record alias into your existing `faroukhasnaoui.tech` hosted zone
- [ ] EventBridge rule `applymind-daily-reminder`, cron `0 8 * * ? *`, targeting Lambda 2

## Manual, one-time, before the first `make cdk-deploy`

1. **Populate `.env` at the repo root** with real values for `NEON_DATABASE_URL`, `APPLYMIND_API_KEY`, and (optional) `OPENAI_API_KEY` / `OPENAI_MODEL`. CDK reads this file at synth time — see `infra/main.go`.
2. **`go mod tidy` inside `infra/`** to resolve `go.sum` for the CDK Go bindings. This sandbox can't reach `proxy.golang.org`, so this hasn't been run yet — do it on your machine first.
3. **`make cdk-bootstrap`** — one-time per AWS account + region (`725927310615` / `eu-west-1`). Creates the CDK staging bucket/roles. Safe to re-run.
4. **Confirm Neon allows Lambda's egress.** Lambda runs outside a VPC here (no VPC in the diagram, no NAT cost), so it connects from AWS's dynamic public IP ranges. Neon's pooled endpoint accepts any source IP over TLS by default — verify this hasn't been restricted on your project's connection settings.
5. **After deploy, verify the ACM certificate validated.** DNS validation is automatic (CDK writes the CNAME into your Route 53 zone), but it can take a few minutes; `cdk deploy` waits for it, so if the command hangs at the certificate step, that's expected — don't cancel early.
6. **DNS propagation for `api.applymind.faroukhasnaoui.tech`.** Usually fast since it's Route 53 alias, but allow a few minutes before relying on the custom domain — use the `ApiDefaultUrl` output in the meantime.

## Not covered by this phase (flagged, not built)
- `RESEND_API_KEY` / actual email delivery — MVP notification delivery is the structured log line read via `GET /notifications/due`, per `cmd/scheduler/main.go`. Nothing currently sends real email.
- `CORS_ALLOWED_ORIGINS` and the S3 CORS rule both currently default to `http://localhost:3000` (or whatever's in your `.env`). Update and redeploy once the Vercel dashboard and the extension have real origins.
- VPC / private networking to Neon — not used; out of scope per the phase's Aurora note.

## Environment variables

| Variable | Required | Notes |
| --- | --- | --- |
| `CORS_ALLOWED_ORIGINS` | yes in deployed envs | Comma-separated, explicit origins. **`*` is rejected at startup.** The API now sends credentials, and a browser refuses a wildcard alongside them. |
| `APPLYMIND_COOKIE_SECURE` | no | Defaults to `true`. Set `false` only for local plain-http development. A `Secure` cookie is silently dropped over http, which presents as "login works, then I am immediately logged out". |

## 1. Migrate

```bash
make migrate-up   # applies 000016_read_only_tokens_and_demo_user
```

Verify:

```sql
\d api_tokens                                     -- is_read_only boolean not null default false
SELECT email, display_name FROM users;            -- seed account + demo@applymind.faroukhasnaoui.tech
```

## 2. Set the seed account's password

The seed account's `password_hash` is a deliberate invalid placeholder, so nothing
can log in as it until this runs. The password comes from stdin, not a flag — a
flag would put it in the process table and in shell history.

```bash
read -rs -p 'password: ' PW && printf '%s' "$PW" | go run ./cmd/setpassword -email mohamdfarouk727@gmail.com
```

If the database is only reachable from somewhere else, generate the hash locally
and apply it by hand:

```bash
read -rs -p 'password: ' PW && printf '%s' "$PW" | go run ./cmd/setpassword -hash-only
```

```sql
UPDATE users SET password_hash = '<hash>', updated_at = now()
WHERE email = 'mohamdfarouk727@gmail.com';
```

Verify: `POST /auth/login` with those credentials returns 200 and a
`applymind_session` cookie.

## 3. Issue the demo account's read-only token

> **This token must be created by hand, and that is deliberate.**
> The demo account's `password_hash` is unusable on purpose — it has no
> interactive login and must never acquire one. It therefore cannot call
> `POST /auth/tokens` itself, and the fix for that is *not* to give the demo
> account a working password: doing so quietly creates an interactive login for
> a shared, publicly-demoed account. Insert the token row directly instead.

```bash
RAW=$(openssl rand -base64 32 | tr '+/' '-_' | tr -d '=')   # base64url, matches generateToken
HASH=$(printf '%s' "$RAW" | openssl dgst -sha256 -hex | awk '{print $2}')
echo "$RAW"   # the only time this value exists — store it server-side, never in the browser
```

```sql
INSERT INTO api_tokens (user_id, token_hash, name, is_read_only)
SELECT id, '<HASH>', 'demo dashboard (read-only)', true
FROM users WHERE email = 'demo@applymind.faroukhasnaoui.tech';
```

Verify, with `$RAW`:

```bash
curl -i -H "Authorization: Bearer $RAW" "$API/auth/me"                  # 200
curl -i -X POST -H "Authorization: Bearer $RAW" "$API/auth/tokens"      # 403 {"error":"read_only_token"}
```

The dashboard holds this token server-side from phase 16. It is never sent to a
browser.

## 4. Post-deploy verification

```bash
curl -i "$API/health"                                    # 200, no credential
curl -i "$API/auth/me"                                   # 401 {"error":"unauthorized"}
curl -i -H "Authorization: Bearer $APPLYMIND_API_KEY" "$API/applications"   # 200 — still the static key this phase
```

## Known for phase 15

- The existing module group still runs on `LegacyStaticKeyAuth`. Phase 15 flips
  it to `RequireAuth` and deletes the legacy function in the same change that
  makes the queries user-aware.
- `seeds/seed-demo-data.sql` still has no `user_id`. It is re-run against the
  demo account in phase 15, once the modules are user-aware.
