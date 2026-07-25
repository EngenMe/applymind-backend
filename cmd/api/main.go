// Command api is the HTTP entry point (Lambda 1: applymind-api).
//
// The same binary runs in two modes:
//   - Under AWS Lambda, detected via AWS_LAMBDA_RUNTIME_API, it serves through
//     the API Gateway proxy adapter.
//   - Locally, it listens on PORT for ordinary HTTP.
//
// Phase 1 wires the router, middleware and database pool only. No module
// routes are registered yet.
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

	"github.com/EngenMe/applymind-backend/pkg/config"
	"github.com/EngenMe/applymind-backend/pkg/database"
	"github.com/EngenMe/applymind-backend/pkg/middleware"
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

	router := newRouter(cfg, pool)

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

func newRouter(cfg *config.Config, pool *pgxpool.Pool) *chi.Mux {
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

			// Phase 1: no module routes registered yet. Each module will expose a
			// RegisterRoutes(protected) call here as it is built.
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
