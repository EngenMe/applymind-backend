// Command api is the HTTP entry point (Lambda 1: applymind-api).
//
// The same binary runs in two modes:
//   - Under AWS Lambda, detected via AWS_LAMBDA_RUNTIME_API, it serves through
//     the API Gateway proxy adapter.
//   - Locally, it listens on PORT for ordinary HTTP.
//
// Phase 5 adds the sites module's routes and startup seeding.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	chiadapter "github.com/awslabs/aws-lambda-go-api-proxy/chi"
	"github.com/go-chi/chi/v5"
	chimiddleware "github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"

	"github.com/EngenMe/applymind-backend/internal/applications"
	"github.com/EngenMe/applymind-backend/internal/coverletters"
	"github.com/EngenMe/applymind-backend/internal/cvs"
	sqlcdb "github.com/EngenMe/applymind-backend/internal/db/sqlc"
	"github.com/EngenMe/applymind-backend/internal/sites"
	"github.com/EngenMe/applymind-backend/pkg/config"
	"github.com/EngenMe/applymind-backend/pkg/database"
	"github.com/EngenMe/applymind-backend/pkg/middleware"
	"github.com/EngenMe/applymind-backend/pkg/storage"
)

var chiLambda *chiadapter.ChiLambda

func main() {
	// .env is a local-development convenience. Under Lambda the variables come
	// from the function's environment, so a missing file is not an error.
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
	pool, err := database.NewPool(ctx, cfg.NeonDatabaseURL)
	if err != nil {
		slog.Error("failed to connect to database", "error", err)
		os.Exit(1)
	}
	defer pool.Close()

	// S3 client for the cvs module. Uses the ambient AWS config — an IAM role
	// under Lambda, or ~/.aws/credentials / env vars locally.
	cvStore, err := storage.NewS3Client(ctx, cfg.CVBucket)
	if err != nil {
		slog.Error("failed to create s3 client", "error", err)
		os.Exit(1)
	}

	queries := sqlcdb.New(pool)

	// Seed the pre-configured site list before the router serves a single
	// request. Ensure-based, so this is safe to run on every cold start: rows
	// already present (LinkedIn, from migration 000010) are left untouched, and
	// only what is missing gets inserted. A failure here means the dashboard
	// settings page and site resolution would come up incomplete, so it is
	// fatal rather than logged-and-ignored.
	siteSvc := sites.NewService(sites.NewRepository(queries))
	if created, err := siteSvc.SeedPreconfigured(ctx); err != nil {
		slog.Error("failed to seed pre-configured sites", "error", err)
		os.Exit(1)
	} else if created > 0 {
		slog.Info("seeded pre-configured sites", "created", created)
	}

	router := newRouter(cfg, pool, queries, cvStore, siteSvc, logger)

	if isLambda() {
		slog.Info("starting in lambda mode")
		chiLambda = chiadapter.New(router)
		lambda.Start(lambdaHandler)
		return
	}

	serveLocal(router, cfg.Port)
}

// isLambda reports whether the process is running inside the Lambda runtime.
// AWS_LAMBDA_RUNTIME_API is injected by the runtime and is absent locally.
func isLambda() bool {
	return os.Getenv("AWS_LAMBDA_RUNTIME_API") != ""
}

func lambdaHandler(ctx context.Context, req events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
	return chiLambda.ProxyWithContext(ctx, req)
}

func newRouter(
	cfg *config.Config,
	pool *pgxpool.Pool,
	queries *sqlcdb.Queries,
	cvStore storage.Client,
	siteSvc sites.Service,
	logger *slog.Logger,
) *chi.Mux {
	r := chi.NewRouter()

	r.Use(chimiddleware.RequestID)
	r.Use(chimiddleware.RealIP)
	r.Use(chimiddleware.Recoverer)
	r.Use(chimiddleware.Timeout(30 * time.Second))

	r.Use(
		cors.Handler(
			cors.Options{
				AllowedOrigins:   cfg.CORSAllowedOrigins,
				AllowedMethods:   []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
				AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type"},
				AllowCredentials: false,
				MaxAge:           300,
			},
		),
	)

	// Unauthenticated: used to confirm the process is up and the database is
	// reachable. Kept outside the API key group so an uptime check does not
	// need to hold a credential.
	r.Get("/health", healthHandler(pool))

	r.Group(
		func(protected chi.Router) {
			protected.Use(middleware.APIKeyAuth(cfg.APIKey))

			cvSvc := cvs.NewService(cvs.NewRepository(queries), cvStore)
			cvs.NewHandler(cvSvc, logger).RegisterRoutes(protected)

			clSvc := coverletters.NewService(coverletters.NewRepository(queries), cvStore)
			coverletters.NewHandler(clSvc, logger).RegisterRoutes(protected)

			// applications takes the pool as well as the queries: it is the first
			// module that opens transactions of its own, so that a save writes the
			// application, its first status history row and its follow-up reminder
			// together or not at all.
			appSvc := applications.NewService(applications.NewRepository(pool, queries), clSvc)
			applications.NewHandler(appSvc, logger).RegisterRoutes(protected)

			// siteSvc is built in main() so it can seed the pre-configured list
			// before the router ever serves a request; here it is only wired to
			// its routes.
			sites.NewHandler(siteSvc, logger).RegisterRoutes(protected)
		},
	)

	return r
}

func healthHandler(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()

		w.Header().Set("Content-Type", "application/json")

		if err := pool.Ping(ctx); err != nil {
			slog.Error("health check: database unreachable", "error", err)
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"status":"unhealthy","database":"unreachable"}`))
			return
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok","database":"reachable"}`))
	}
}

// serveLocal runs an ordinary HTTP server with graceful shutdown on SIGINT or
// SIGTERM, so local runs release the port and close the pool cleanly.
func serveLocal(router *chi.Mux, port string) {
	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		slog.Info("starting local http server", "port", port)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("http server failed", "error", err)
			os.Exit(1)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("graceful shutdown failed", "error", err)
	}
}
