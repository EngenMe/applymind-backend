.PHONY: migrate-up migrate-down migrate-drop migrate-version migrate-create sqlc test run-api run-scheduler

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
