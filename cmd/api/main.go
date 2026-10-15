// Command api runs the HTTP API.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ronybrand/go-order-api/internal/app"
	"github.com/ronybrand/go-order-api/internal/auth"
	"github.com/ronybrand/go-order-api/internal/config"
	"github.com/ronybrand/go-order-api/internal/httpapi"
	"github.com/ronybrand/go-order-api/internal/messaging"
	"github.com/ronybrand/go-order-api/internal/postgres"
)

const (
	readHeaderTimeout = 5 * time.Second
	readTimeout       = 15 * time.Second
	writeTimeout      = 30 * time.Second
	idleTimeout       = 60 * time.Second
	maxHeaderBytes    = 16 << 10
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.ErrorContext(context.Background(), "api stopped", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}
	if err := cfg.ValidateAPI(); err != nil {
		return fmt.Errorf("configuration: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("database: %w", err)
	}
	defer pool.Close()
	if err := postgres.Migrate(ctx, pool, logger); err != nil {
		return fmt.Errorf("migrations: %w", err)
	}

	verifier, err := auth.LoadVerifier(cfg.JWTPublicKeyPath, cfg.JWTAudience, cfg.JWTIssuer)
	if err != nil {
		return fmt.Errorf("jwt: %w", err)
	}

	store := postgres.NewStore(pool)
	var publisher app.EventPublisher
	if cfg.BrokerURL != "" {
		p := messaging.NewPublisher(cfg.BrokerURL)
		defer p.Close() //nolint:errcheck // closing on the way out
		publisher = p
	}

	handler := httpapi.New(httpapi.Options{
		Customers: app.NewCustomerService(store, app.WithLogger(logger)),
		Orders:    app.NewOrderService(store, publisher, app.WithLogger(logger)),
		Verifier:  verifier, Logger: logger, Ping: pool.Ping,
		CORSOrigins: cfg.CORSOrigins, MaxBodyBytes: cfg.MaxBodyBytes,
		RateLimitPerUser: cfg.RateLimitPerUser, RateLimitWrites: cfg.RateLimitWrites, RateLimitPerIP: cfg.RateLimitPerIP,
	})
	server := &http.Server{
		Addr: cfg.HTTPAddr, Handler: handler,
		ReadHeaderTimeout: readHeaderTimeout, ReadTimeout: readTimeout, WriteTimeout: writeTimeout,
		IdleTimeout: idleTimeout, MaxHeaderBytes: maxHeaderBytes,
		ErrorLog: slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}

	failed := make(chan error, 1)
	go func() {
		logger.InfoContext(ctx, "listening", "addr", cfg.HTTPAddr, "environment", cfg.Environment)
		if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			failed <- err
		}
	}()

	select {
	case err := <-failed:
		return err
	case <-ctx.Done():
	}
	logger.InfoContext(ctx, "shutting down")
	// A fresh context: ctx is already canceled, and in-flight requests deserve their grace period.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	return server.Shutdown(shutdownCtx)
}
