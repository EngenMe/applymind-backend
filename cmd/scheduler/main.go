// Command scheduler is the reminder entry point (Lambda 2:
// applymind-scheduler), invoked by EventBridge on cron 0 8 * * ? *.
//
// The same binary runs in two modes:
//   - Under AWS Lambda it registers run() as the event handler.
//   - Locally it executes run() exactly once and exits, so a scheduled pass
//     can be exercised on demand without waiting for the cron.
//
// Phase 1 establishes the entry point and its dependencies only. The reminder
// sweep itself belongs to the notifications module in a later phase.
package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"

	"github.com/EngenMe/applymind-backend/pkg/config"
	"github.com/EngenMe/applymind-backend/pkg/database"
)

var pool *pgxpool.Pool

func main() {
	_ = godotenv.Load()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	cfg, err := config.Load()
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	ctx := context.Background()
	pool, err = database.NewPool(ctx, cfg.NeonDatabaseURL)
	if err != nil {
		slog.Error("failed to connect to database", "error", err)
		os.Exit(1)
	}
	defer pool.Close()

	if isLambda() {
		slog.Info("starting scheduler in lambda mode")
		lambda.Start(handler)
		return
	}

	slog.Info("running scheduler once (local mode)")
	if err := run(ctx); err != nil {
		slog.Error("scheduler run failed", "error", err)
		os.Exit(1)
	}
	slog.Info("scheduler run complete")
}

func isLambda() bool {
	return os.Getenv("AWS_LAMBDA_RUNTIME_API") != ""
}

func handler(ctx context.Context, event events.CloudWatchEvent) error {
	slog.Info("scheduled invocation received", "event_id", event.ID, "time", event.Time)
	return run(ctx)
}

// run performs one reminder sweep.
//
// Phase 1 intentionally does nothing beyond proving the entry point boots and
// the database is reachable. The actual query for due reminders and the Resend
// delivery call are out of scope for this phase.
func run(ctx context.Context) error {
	if err := pool.Ping(ctx); err != nil {
		return err
	}
	slog.Info("scheduler: no work implemented in phase 1")
	return nil
}
