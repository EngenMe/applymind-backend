.PHONY: migrate-up migrate-down migrate-drop migrate-version migrate-create sqlc test run-api run-scheduler \
	build-api build-scheduler build-lambdas cdk-bootstrap cdk-synth cdk-diff cdk-deploy cdk-destroy

# Migrations run against the DIRECT (non-pooled) Neon endpoint. golang-migrate
# takes a session-level advisory lock, which does not survive PgBouncer's
# transaction pooling on the -pooler hostname.
include .env
export

MIGRATIONS_DIR := migrations

migrate-up:
	migrate -path $(MIGRATIONS_DIR) -database "$(NEON_DIRECT_URL)" up

migrate-down:
	migrate -path $(MIGRATIONS_DIR) -database "$(NEON_DIRECT_URL)" down 1

migrate-drop:
	migrate -path $(MIGRATIONS_DIR) -database "$(NEON_DIRECT_URL)" drop -f

migrate-version:
	migrate -path $(MIGRATIONS_DIR) -database "$(NEON_DIRECT_URL)" version

# Usage: make migrate-create NAME=add_something
migrate-create:
	migrate create -ext sql -dir $(MIGRATIONS_DIR) -seq $(NAME)

sqlc:
	sqlc generate

test:
	go test ./...

run-api:
	go run ./cmd/api

run-scheduler:
	go run ./cmd/scheduler

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
