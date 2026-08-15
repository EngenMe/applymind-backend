.PHONY: migrate-up migrate-down migrate-drop migrate-version migrate-create sqlc build test test-scripts run-api run-scheduler \
	build-api build-scheduler build-lambdas cdk-bootstrap cdk-synth cdk-diff cdk-deploy cdk-destroy

SHELL := /bin/bash

MIGRATIONS_DIR := migrations
SCRIPTS_DIR    := scripts

PORT ?= 8080
HEALTH_URL := http://localhost:$(PORT)/health

# .env is loaded per-recipe with bash's own `source`, not with Make's `include`.
#
# Make's `include` parses a file as makefile syntax: it expands `$` as the
# start of a variable reference, doesn't strip quotes the way a real .env
# parser does, and disagrees with godotenv (which cmd/api uses) on anything
# that isn't a bare alphanumeric value. Since godotenv.Load() never overrides
# an already-set environment variable, a value Make mis-parsed and exported
# would silently win over the binary's own correct parsing — and every script
# that independently `source`s .env, or is run by a human who already has it
# exported normally, would see the real value while the server saw something
# else. That split is exactly what produced every legacy-key route returning
# 401 while nothing about the auth module was wrong.
#
# `source ./.env` inside the recipe is the same parsing a person or a script
# gets by running `source .env` themselves, so the server and the scripts are
# guaranteed to agree.
define load_env
	set -a; [ -f .env ] && source ./.env; set +a
endef

migrate-up:
	@$(load_env); migrate -path $(MIGRATIONS_DIR) -database "$$NEON_DATABASE_URL" up

migrate-down:
	@$(load_env); migrate -path $(MIGRATIONS_DIR) -database "$$NEON_DATABASE_URL" down 1

migrate-drop:
	@$(load_env); migrate -path $(MIGRATIONS_DIR) -database "$$NEON_DATABASE_URL" drop -f

migrate-version:
	@$(load_env); migrate -path $(MIGRATIONS_DIR) -database "$$NEON_DATABASE_URL" version

# Usage: make migrate-create NAME=add_something
migrate-create:
	migrate create -ext sql -dir $(MIGRATIONS_DIR) -seq $(NAME)

sqlc:
	sqlc generate

# Compiles every package for the host platform, including the ones with no
# tests — cmd/api, cmd/setpassword, pkg/config, pkg/storage and the rest. `make
# test` builds those too as a side effect of `go test ./...`, but it reports
# them as "no test files", which reads as "nothing happened" rather than "this
# compiles". Vet runs alongside because the mistakes it catches (a printf verb
# that doesn't match its argument, a lost struct tag) compile perfectly well.
build:
	go build ./...
	go vet ./...

test:
	go test ./...

run-api:
	@$(load_env); go run ./cmd/api

run-scheduler:
	@$(load_env); go run ./cmd/scheduler

# --- Shell-script integration tests -----------------------------------------
#
# Runs every scripts/*.sh file against a live server: build a local binary
# (host OS/arch, not the Lambda arm64 target build-api produces), start it in
# the background, wait for /health, run each script in sorted order, then kill
# the server unconditionally via trap — on a clean finish, on a failed script,
# or on Ctrl-C.
#
# .env is sourced once, here, with bash — not via Make's `include` — so the
# server binary and every script below see byte-identical values for anything
# .env defines. See the load_env comment above for why that distinction matters.
#
# A single local binary rather than `go run` in the background: `go run`
# leaves the compiled process as a child of a wrapper process, so killing the
# PID `$!` captures does not reliably kill the actual server underneath it,
# and a failed run here would leak a process still holding $(PORT).
test-scripts:
	@set -eo pipefail; \
	$(load_env); \
	echo "Checking for a leftover process on :$(PORT)..."; \
	if command -v lsof >/dev/null 2>&1; then \
		EXISTING_PIDS=$$(lsof -ti tcp:$(PORT) 2>/dev/null || true); \
	else \
		echo "  (lsof not found — skipping preflight kill; relying on the post-boot PID check below)"; \
		EXISTING_PIDS=""; \
	fi; \
	if [ -n "$$EXISTING_PIDS" ]; then \
		echo "  Found $$EXISTING_PIDS still bound to :$(PORT) — killing it before starting a fresh server."; \
		kill $$EXISTING_PIDS 2>/dev/null || true; \
		sleep 1; \
		kill -9 $$EXISTING_PIDS 2>/dev/null || true; \
	fi; \
	BIN=$$(mktemp /tmp/applymind-api-test.XXXXXX); \
	echo "Building API binary..."; \
	go build -o "$$BIN" ./cmd/api; \
	echo "Starting API server on :$(PORT) (pid will follow)..."; \
	"$$BIN" & \
	SERVER_PID=$$!; \
	cleanup() { \
		echo ""; \
		echo "Shutting down API server (pid $$SERVER_PID)..."; \
		kill $$SERVER_PID 2>/dev/null || true; \
		wait $$SERVER_PID 2>/dev/null || true; \
		rm -f "$$BIN"; \
	}; \
	trap cleanup EXIT INT TERM; \
	echo "Waiting for $(HEALTH_URL) ..."; \
	up=0; \
	for i in $$(seq 1 30); do \
		if ! kill -0 $$SERVER_PID 2>/dev/null; then \
			echo "Server process (pid $$SERVER_PID) exited before becoming healthy — check the build/boot log above."; \
			exit 1; \
		fi; \
		if curl -sf "$(HEALTH_URL)" > /dev/null 2>&1; then \
			up=1; break; \
		fi; \
		sleep 1; \
	done; \
	if [ "$$up" -ne 1 ]; then \
		echo "Server did not become healthy within 30s."; \
		exit 1; \
	fi; \
	echo "Server is up (pid $$SERVER_PID)."; \
	if [ -n "$$APPLYMIND_API_KEY" ]; then \
		SANITY=$$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $$APPLYMIND_API_KEY" "http://localhost:$(PORT)/sites"); \
		if [ "$$SANITY" != "200" ]; then \
			echo ""; \
			echo "WARNING: a static-key request to /sites returned $$SANITY, not 200."; \
			echo "  Every script below that hits a legacy-key-protected route will fail the same way."; \
			echo "  APPLYMIND_API_KEY as loaded here is $${#APPLYMIND_API_KEY} characters, starting"; \
			echo "  with '$${APPLYMIND_API_KEY:0:6}...' — compare that against what's actually in .env"; \
			echo "  and against what the scripts below send."; \
			echo ""; \
		fi; \
	else \
		echo "WARNING: APPLYMIND_API_KEY is empty after sourcing .env — check that .env exists and defines it."; \
	fi; \
	PASS=0; FAIL=0; FAILED=""; \
	shopt -s nullglob; \
	SCRIPT_LIST=$$(find $(SCRIPTS_DIR) -maxdepth 1 -name '*.sh' | sort); \
	if [ -z "$$SCRIPT_LIST" ]; then \
		echo "No *.sh files found in $(SCRIPTS_DIR)/"; \
		exit 1; \
	fi; \
	for script in $$SCRIPT_LIST; do \
		echo ""; \
		echo "=== $$script ==="; \
		if BASE_URL="http://localhost:$(PORT)" API_KEY="$$APPLYMIND_API_KEY" bash "$$script"; then \
			echo "--- PASS: $$script"; \
			PASS=$$((PASS+1)); \
		else \
			echo "--- FAIL: $$script (exit $$?)"; \
			FAIL=$$((FAIL+1)); \
			FAILED="$$FAILED  - $$script\n"; \
		fi; \
	done; \
	echo ""; \
	echo "======================================"; \
	echo " Script test report"; \
	echo "   Passed: $$PASS"; \
	echo "   Failed: $$FAIL"; \
	if [ -n "$$FAILED" ]; then \
		echo ""; \
		echo " Failing scripts:"; \
		echo -e "$$FAILED"; \
	fi; \
	echo "======================================"; \
	if [ $$FAIL -ne 0 ]; then exit 1; fi

# --- Phase 8: AWS CDK deploy -------------------------------------------------
# Lambda's provided.al2023 runtime execs a file literally named `bootstrap`.
# CDK doesn't cross-compile Go itself, so these targets build the binaries
# first; cdk-synth/diff/deploy depend on build-lambdas so the assets in
# infra/lambda/{api,scheduler}/ are never stale when CDK reads them.

build-api:
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o infra/lambda/api/bootstrap ./cmd/api

build-scheduler:
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o infra/lambda/scheduler/bootstrap ./cmd/scheduler

build-lambdas: build-api build-scheduler

# One-time per AWS account+region. Safe to re-run; it's idempotent.
cdk-bootstrap:
	cd infra && npx cdk bootstrap aws://$$(aws sts get-caller-identity --query Account --output text)/eu-west-1

cdk-synth: build-lambdas
	cd infra && npx cdk synth

cdk-diff: build-lambdas
	cd infra && npx cdk diff

cdk-deploy: build-lambdas
	cd infra && npx cdk deploy

cdk-destroy:
	cd infra && npx cdk destroy