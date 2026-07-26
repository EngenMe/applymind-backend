// Command scheduler is the reminder entry point (Lambda 2:
// applymind-scheduler), invoked by EventBridge on cron 0 8 * * ? *.
//
// The same binary runs in two modes:
//   - Under AWS Lambda it registers run() as the event handler.
//   - Locally it executes run() exactly once and exits, so a scheduled pass
//     can be exercised on demand without waiting for the cron.
//
// Phase 6 wires the sweep itself: one invocation is one call to
// notifications.Service.CheckReminders — flow 4 phases 2 to 4. Delivery goes
// through the module's Notifier port, which in the MVP writes a structured log
// line; the user-visible browser notification is raised by the dashboard from
// GET /notifications/due.
package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"

	sqlcdb "github.com/EngenMe/applymind-backend/internal/db/sqlc"
	"github.com/EngenMe/applymind-backend/internal/notifications"
	"github.com/EngenMe/applymind-backend/pkg/config"
	"github.com/EngenMe/applymind-backend/pkg/database"
)

var (
	pool     *pgxpool.Pool
	notifSvc notifications.Service
)

func main() {
	_ = godotenv.Load()

	logger := slog.New(
		slog.NewJSONHandler(
			os.Stdout, &slog.HandlerOptions{
				Level: slog.LevelInfo,
			},
		),
	)
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

	// The sweep never opens a transaction of its own — each reminder is stamped
	// independently, so one failure cannot roll back the reminders before it —
	// so the repository takes only the queries, not the pool.
	notifSvc = notifications.NewService(
		notifications.NewRepository(sqlcdb.New(pool)),
		notifications.WithNotifier(notifications.NewLogNotifier(logger)),
		notifications.WithLogger(logger),
	)

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
}

func isLambda() bool {
	return os.Getenv("AWS_LAMBDA_RUNTIME_API") != ""
}

func handler(ctx context.Context, event events.CloudWatchEvent) error {
	slog.Info("scheduled invocation received", "event_id", event.ID, "time", event.Time)
	return run(ctx)
}

// run performs one reminder sweep — flow 4 steps 2 to 12.
//
// An error here fails the invocation, which is what makes EventBridge's retry
// meaningful: the sweep could not read the due list, so nothing was raised.
// Per-reminder failures are handled inside the service and reported in the
// summary rather than failing the run, because the reminders that did go out
// should not be repeated tomorrow just because one of their neighbours failed.
func run(ctx context.Context) error {
	slog.InfoContext(ctx, "scheduler invoked, starting follow-up reminder check")

	summary, err := notifSvc.CheckReminders(ctx)
	if err != nil {
		return err
	}

	// Flow 4 step 12: the run summary as structured JSON.
	slog.InfoContext(
		ctx, "scheduler complete",
		slog.String("event", "scheduler_complete"),
		slog.Int("due_count", summary.DueCount),
		slog.Int("sent_count", summary.SentCount),
		slog.Int("failed_count", summary.FailedCount),
		slog.Int64("duration_ms", summary.Duration.Milliseconds()),
	)
	return nil
}
