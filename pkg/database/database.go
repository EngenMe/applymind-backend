// Package database manages the Postgres connection pool to Neon.
// Lambda-specific note: the pool is created once per Lambda execution
// environment (cold start) and reused across warm invocations, which is
// why NewPool takes a context.Context but the returned pool is stored on
// a package-level or handler-level variable by the caller, not recreated
// per-request.
package database

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// NewPool creates a pgx connection pool against the given Postgres URL
// (expected to be Neon's pooled/PgBouncer connection string) and verifies
// connectivity with a ping before returning.
func NewPool(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("database: parse config: %w", err)
	}

	poolCfg.MaxConns = 5
	poolCfg.MinConns = 0
	poolCfg.MaxConnIdleTime = 30 * time.Second

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("database: create pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("database: ping failed: %w", err)
	}

	return pool, nil
}
