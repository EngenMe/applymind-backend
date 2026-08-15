// Command api is the HTTP entry point (Lambda 1: applymind-api).
//
// The same binary runs in two modes:
//   - Under AWS Lambda, detected via AWS_LAMBDA_RUNTIME_API, it serves through
//     the API Gateway proxy adapter.
//   - Locally, it listens on PORT for ordinary HTTP.
//
// Phase 14 added the auth module: /auth/register and /auth/login are public,
// the rest of /auth/* resolves a real user from a session cookie or an API
// token. Phase 15 folded every other module into the same authenticated
// group and deleted the static-key middleware that used to guard them —
// see pkg/middleware/auth.go.
package main

import (
	"context"
	"encoding/json"
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
	"github.com/EngenMe/applymind-backend/internal/auth"
	"github.com/EngenMe/applymind-backend/internal/coverletters"
	"github.com/EngenMe/applymind-backend/internal/cvs"
	sqlcdb "github.com/EngenMe/applymind-backend/internal/db/sqlc"
	"github.com/EngenMe/applymind-backend/internal/notifications"
	"github.com/EngenMe/applymind-backend/internal/settings"
	"github.com/EngenMe/applymind-backend/internal/sites"
	"github.com/EngenMe/applymind-backend/pkg/ai"
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

	// The OpenAI client is optional. Without a key the API behaves exactly as it
	// did in phase 6: applications save, ai_score stays NULL. With a bad key it
	// still starts — the failure surfaces per save, fail-soft, rather than
	// taking the whole function down.
	var scorer applications.Scorer
	if cfg.AIScoringEnabled() {
		client, err := ai.NewClient(
			cfg.OpenAIAPIKey,
			ai.WithModel(cfg.OpenAIModel),
			ai.WithTimeout(cfg.AIScoreTimeout),
		)
		if err != nil {
			slog.Error("failed to create openai client", "error", err)
			os.Exit(1)
		}
		scorer = client
		slog.Info("ai job scoring enabled", "model", client.Model(), "timeout", cfg.AIScoreTimeout)
	} else {
		slog.Warn("OPENAI_API_KEY is not set — applications will be saved without an ai match score")
	}

	if !cfg.CookieSecure {
		// Worth a line in the log, because a session cookie without Secure
		// travels over plain http. Expected locally and nowhere else, so the
		// warning is what makes it visible if it ever ships.
		slog.Warn("session cookies are being issued without the Secure flag — local http development only")
	}

	router := newRouter(cfg, pool, queries, cvStore, siteSvc, scorer, logger)

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
	scorer applications.Scorer,
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
				AllowedOrigins: cfg.CORSAllowedOrigins,
				AllowedMethods: []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
				AllowedHeaders: []string{"Accept", "Authorization", "Content-Type"},
				// Required from phase 14: the dashboard authenticates with the
				// applymind_session cookie, and a browser will neither send nor
				// store it cross-origin unless credentials are allowed. Without
				// this, login appears to succeed and every request after it is
				// a 401.
				//
				// The cost is that AllowedOrigins can never be "*" — a browser
				// refuses a wildcard alongside credentials. config.Load rejects
				// "*" at startup so that constraint fails next to the env var
				// that broke it rather than in a browser console.
				AllowCredentials: true,
				MaxAge:           300,
			},
		),
	)

	// Unauthenticated: used to confirm the process is up and the database is
	// reachable. Kept outside every auth group so an uptime check does not need
	// to hold a credential.
	r.Get("/health", healthHandler(pool))

	authSvc := auth.NewService(auth.NewRepository(queries))
	authHandler := auth.NewHandler(authSvc, logger, auth.WithSecureCookies(cfg.CookieSecure))

	// Public: the two endpoints that cannot require a session, because they are
	// how a session is obtained.
	authHandler.RegisterPublicRoutes(r)

	// Everything else under /auth/* resolves a real user — cookie for the
	// dashboard, bearer token for the extension. Every other module lives in
	// the same group: phase 15 made their queries user-aware, so there is no
	// module left that needs a different (or no) credential.
	r.Group(
		func(authed chi.Router) {
			authed.Use(middleware.RequireAuth(authSvc, logger))
			authHandler.RegisterProtectedRoutes(authed, middleware.UserIDFromRequest)

			cvSvc := cvs.NewService(cvs.NewRepository(queries), cvStore)
			cvs.NewHandler(cvSvc, logger).RegisterRoutes(authed)

			clSvc := coverletters.NewService(coverletters.NewRepository(queries), cvStore)
			coverletters.NewHandler(clSvc, logger).RegisterRoutes(authed)

			// settings owns the profile summary the AI score is calculated
			// against, so it is built before applications and handed to it.
			settingsSvc := settings.NewService(settings.NewRepository(queries))
			settings.NewHandler(settingsSvc, logger).RegisterRoutes(authed)

			// applications takes the pool as well as the queries: it is the first
			// module that opens transactions of its own, so that a save writes the
			// application, its first status history row and its follow-up reminder
			// together or not at all.
			appOpts := []applications.ServiceOption{applications.WithLogger(logger)}
			if scorer != nil {
				// Scoring only turns on with both halves present: a model to ask
				// and a profile to ask about.
				appOpts = append(
					appOpts,
					applications.WithScoring(scorer, settingsSvc),
					applications.WithScoreTimeout(cfg.AIScoreTimeout),
				)
			}
			appSvc := applications.NewService(applications.NewRepository(pool, queries), clSvc, appOpts...)
			applications.NewHandler(appSvc, logger).RegisterRoutes(authed)

			// siteSvc is built in main() so it can seed the pre-configured list
			// before the router ever serves a request; here it is only wired to
			// its routes.
			sites.NewHandler(siteSvc, logger).RegisterRoutes(authed)

			// notifications is the same service the scheduler runs, but Lambda 1
			// only reads through it: GET /notifications/due tells the dashboard
			// what to raise a browser notification about. It is constructed
			// without a Notifier because nothing on this path dispatches — the
			// default LogNotifier is never reached from ListDue.
			notifSvc := notifications.NewService(
				notifications.NewRepository(queries),
				notifications.WithLogger(logger),
			)
			notifications.NewHandler(notifSvc, logger).RegisterRoutes(authed)
		},
	)

	// chi bakes a group's middleware onto each handler it registers, not onto
	// the router as a whole. A method with no registered handler anywhere —
	// this API has no PUT route at all, for instance — never reaches
	// RequireAuth; chi answers 405 straight out of its routing tree first,
	// with an Allow header that discloses which methods do exist at that
	// path. That is more than a read-only credential should learn, and the
	// phase's own test list requires a write attempt to get the same 403
	// read_only_token a supported write gets, regardless of whether a handler
	// happens to exist. This is the one place that has to run before routing
	// decides a method doesn't exist, which is why it is wired at the Mux
	// root rather than inside a group.
	//
	// Every credential that is not a read-only token — no credential, a
	// full-access session or token — is unaffected: this only changes the
	// response when CheckReadOnlyWrite's own identity resolution succeeds and
	// finds IsReadOnly, and it costs one DB lookup, so it only runs at all
	// when a request actually presents something that looks like a
	// credential.
	r.MethodNotAllowed(readOnlyMethodNotAllowed(authSvc))

	return r
}

// readOnlyMethodNotAllowed is the router-wide fallback for a method that has
// no handler registered at that path anywhere in the app. See the comment
// where this is wired in newRouter for why it has to live here rather than
// inside the auth group.
func readOnlyMethodNotAllowed(authSvc auth.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if middleware.CheckReadOnlyWrite(r, authSvc) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "read_only_token"})
			return
		}
		// Everyone else — no credential, the static key, a full-access
		// session or token — sees chi's ordinary response: unchanged from
		// before this existed.
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
	}
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
