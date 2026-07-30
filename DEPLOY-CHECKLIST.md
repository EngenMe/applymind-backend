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
