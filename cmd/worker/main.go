// Command worker consumes order status changes and sends the notification e-mails.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/ronybrand/go-order-api/internal/config"
	"github.com/ronybrand/go-order-api/internal/messaging"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("worker stopped", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}
	if err := cfg.ValidateWorker(); err != nil {
		return fmt.Errorf("configuration: %w", err)
	}

	mailer, err := messaging.NewSMTPMailer(messaging.SMTPConfig{
		Host: cfg.SMTPHost, Port: cfg.SMTPPort, User: cfg.SMTPUser, Password: cfg.SMTPPassword,
		From: cfg.SMTPFrom, RequireTLS: cfg.IsProduction(),
	})
	if err != nil {
		return fmt.Errorf("smtp: %w", err)
	}
	notifier, err := messaging.NewEmailNotifier(mailer)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	consumer := messaging.NewConsumer(cfg.BrokerURL, messaging.NewProcessor(notifier, logger), logger)
	return consumer.Run(ctx)
}
