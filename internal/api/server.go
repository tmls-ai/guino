package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	chimiddleware "github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"

	"github.com/tmls-ai/guino/internal/api/handlers"
	"github.com/tmls-ai/guino/internal/api/middleware"
	"github.com/tmls-ai/guino/internal/api/ws"
	"github.com/tmls-ai/guino/internal/config"
	"github.com/tmls-ai/guino/internal/dashboard"
	"github.com/tmls-ai/guino/internal/engine"
	"github.com/tmls-ai/guino/internal/runtime"
)

// Server is the HTTP API server.
type Server struct {
	httpServer  *http.Server
	engine      *engine.Engine
	runtime     runtime.Runtime
	config      *config.Config
	logger      *slog.Logger
	rateLimiter *middleware.RateLimiter
}

// NewServer creates a new API server.
func NewServer(eng *engine.Engine, rt runtime.Runtime, cfg *config.Config, logger *slog.Logger) *Server {
	rl := middleware.NewRateLimiter(cfg.Server.RateLimitRPS, cfg.Server.RateLimitBurst)

	s := &Server{
		engine:      eng,
		runtime:     rt,
		config:      cfg,
		logger:      logger,
		rateLimiter: rl,
	}

	r := chi.NewRouter()

	// Middleware stack
	r.Use(chimiddleware.RequestID)
	r.Use(chimiddleware.RealIP)
	r.Use(middleware.Logger(logger))
	r.Use(chimiddleware.Recoverer)
	r.Use(rl.Middleware())
	allowedOrigins := cfg.Server.AllowedOrigins
	if len(allowedOrigins) == 0 {
		allowedOrigins = []string{"http://localhost:8080", "http://127.0.0.1:8080"}
	}
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins:   allowedOrigins,
		AllowedMethods:   []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type", "X-API-Key"},
		ExposedHeaders:   []string{"Link"},
		AllowCredentials: false,
		MaxAge:           300,
	}))

	// Register routes
	RegisterRoutes(r, eng, rt, cfg, logger)

	// Mount dashboard at root
	r.Handle("/*", dashboard.Handler())

	s.httpServer = &http.Server{
		Addr:         fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port),
		Handler:      r,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 120 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	return s
}

// Start starts the HTTP server.
func (s *Server) Start() error {
	s.logger.Info("starting API server", "addr", s.httpServer.Addr)
	if err := s.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// Shutdown gracefully stops the server.
func (s *Server) Shutdown(ctx context.Context) error {
	s.rateLimiter.Close()
	return s.httpServer.Shutdown(ctx)
}

// RegisterRoutes sets up all API routes.
func RegisterRoutes(r chi.Router, eng *engine.Engine, rt runtime.Runtime, cfg *config.Config, logger *slog.Logger) {
	sh := handlers.NewSandboxHandler(eng, logger)
	eh := handlers.NewExecHandler(eng, logger)
	fh := handlers.NewFileHandler(eng, logger)
	ph := handlers.NewPortHandler(eng, logger)
	snapH := handlers.NewSnapshotHandler(eng, logger)
	statsH := handlers.NewStatsHandler(eng, logger)
	resH := handlers.NewResourceHandler(eng, logger)
	s3H := handlers.NewS3Handler(eng, cfg.S3, logger)
	wsH := ws.NewExecHandler(eng, logger, cfg.Server.AllowedOrigins)

	// Health & version — outside auth so load balancers and K8s probes can access them
	r.Get("/api/v1/health", handlers.HealthHandler(rt))
	r.Get("/api/v1/version", handlers.Version)

	r.Route("/api/v1", func(r chi.Router) {
		// Auth middleware (conditional)
		if cfg.Auth.Enabled {
			r.Use(middleware.Auth(cfg.Auth.APIKeys))
		}

		// Sandbox CRUD
		r.Post("/sandboxes", sh.Create)
		r.Get("/sandboxes", sh.List)
		r.Get("/sandboxes/{id}", sh.Get)
		r.Delete("/sandboxes/{id}", sh.Delete)
		r.Post("/sandboxes/{id}/stop", sh.Stop)
		r.Post("/sandboxes/{id}/start", sh.Start)

		// Exec
		r.Post("/sandboxes/{id}/exec", eh.Exec)

		// File operations
		r.Get("/sandboxes/{id}/files", fh.ReadFile)
		r.Put("/sandboxes/{id}/files", fh.WriteFile)
		r.Get("/sandboxes/{id}/files/list", fh.ListDir)
		r.Get("/sandboxes/{id}/files/stat", fh.Stat)
		r.Post("/sandboxes/{id}/files/mkdir", fh.MkDir)
		r.Delete("/sandboxes/{id}/files", fh.RemoveFile)
		r.Post("/sandboxes/{id}/files/upload", fh.Upload)
		r.Get("/sandboxes/{id}/files/download", fh.Download)

		// S3 operations
		r.Post("/sandboxes/{id}/files/s3-import", s3H.Import)
		r.Post("/sandboxes/{id}/files/s3-export", s3H.Export)

		// Port forwarding
		r.Get("/sandboxes/{id}/ports", ph.List)
		r.Post("/sandboxes/{id}/ports", ph.Add)
		r.Delete("/sandboxes/{id}/ports/{port}", ph.Remove)

		// Snapshots
		r.Post("/sandboxes/{id}/snapshots", snapH.Create)
		r.Get("/sandboxes/{id}/snapshots", snapH.List)
		r.Post("/snapshots/{snapshotId}/restore", snapH.Restore)
		r.Delete("/snapshots/{snapshotId}", snapH.Delete)

		// Stats
		r.Get("/sandboxes/{id}/stats", statsH.SandboxStats)
		r.Get("/stats", statsH.SystemStats)

		// Resource status
		r.Get("/resources", resH.Status)

		// WebSocket exec
		r.Get("/sandboxes/{id}/exec/stream", wsH.Handle)
	})
}
