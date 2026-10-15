// Package config loads the service settings from environment variables and refuses to start in production
// with an unsafe combination (fail fast beats a quietly insecure deployment).
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Environment names.
const (
	EnvDevelopment = "development"
	EnvProduction  = "production"
)

// Config is the whole configuration of the API and the worker.
type Config struct {
	Environment string
	HTTPAddr    string

	DatabaseURL string

	JWTPublicKeyPath string
	JWTAudience      string
	JWTIssuer        string

	CORSOrigins      []string
	MaxBodyBytes     int64
	RateLimitPerUser int // requests per minute, any method
	RateLimitWrites  int // requests per minute, unsafe methods only
	RateLimitPerIP   int // requests per minute, coarse limit before authentication

	BrokerURL string

	SMTPHost     string
	SMTPPort     int
	SMTPUser     string
	SMTPPassword string
	SMTPFrom     string

	ShutdownTimeout time.Duration
}

// IsProduction reports whether the strict production rules apply.
func (c Config) IsProduction() bool { return c.Environment == EnvProduction }

// Load reads the configuration from the process environment.
func Load() (Config, error) { return load(os.LookupEnv) }

func load(lookup func(string) (string, bool)) (Config, error) {
	r := reader{lookup: lookup}
	cfg := Config{
		Environment:      r.str("APP_ENV", EnvDevelopment),
		HTTPAddr:         r.str("HTTP_ADDR", ":8080"),
		DatabaseURL:      r.str("DATABASE_URL", ""),
		JWTPublicKeyPath: r.str("JWT_PUBLIC_KEY_PATH", ""),
		JWTAudience:      r.str("JWT_AUDIENCE", ""),
		JWTIssuer:        r.str("JWT_ISSUER", ""),
		CORSOrigins:      r.list("CORS_ORIGINS"),
		MaxBodyBytes:     int64(r.integer("MAX_BODY_BYTES", 1<<20)),
		RateLimitPerUser: r.integer("RATE_LIMIT_PER_USER", 120),
		RateLimitWrites:  r.integer("RATE_LIMIT_WRITES", 30),
		RateLimitPerIP:   r.integer("RATE_LIMIT_PER_IP", 600),
		BrokerURL:        r.str("BROKER_URL", ""),
		SMTPHost:         r.str("SMTP_HOST", ""),
		SMTPPort:         r.integer("SMTP_PORT", 587),
		SMTPUser:         r.str("SMTP_USER", ""),
		SMTPPassword:     r.str("SMTP_PASSWORD", ""),
		SMTPFrom:         r.str("SMTP_FROM", ""),
		ShutdownTimeout:  time.Duration(r.integer("SHUTDOWN_TIMEOUT_SECONDS", 15)) * time.Second,
	}
	if err := errors.Join(r.errs...); err != nil {
		return Config{}, err
	}
	return cfg, cfg.validate()
}

// validate checks what both processes need; ValidateAPI and ValidateWorker add the rest.
func (c Config) validate() error {
	var errs []error
	if c.Environment != EnvDevelopment && c.Environment != EnvProduction {
		errs = append(errs, fmt.Errorf("APP_ENV must be %q or %q", EnvDevelopment, EnvProduction))
	}
	if c.DatabaseURL == "" {
		errs = append(errs, errors.New("DATABASE_URL is required"))
	}
	if c.MaxBodyBytes < 1 {
		errs = append(errs, errors.New("MAX_BODY_BYTES must be positive"))
	}
	if c.IsProduction() {
		errs = append(errs, c.productionRules()...)
	}
	return errors.Join(errs...)
}

func (c Config) productionRules() []error {
	var errs []error
	if c.JWTAudience == "" || c.JWTIssuer == "" {
		errs = append(errs, errors.New("JWT_AUDIENCE and JWT_ISSUER are required in production"))
	}
	if len(c.CORSOrigins) == 0 {
		errs = append(errs, errors.New("CORS_ORIGINS must list the allowed origins in production"))
	}
	for _, origin := range c.CORSOrigins {
		if origin == "*" {
			errs = append(errs, errors.New("CORS_ORIGINS must not contain * in production"))
		}
	}
	if c.BrokerURL != "" {
		if u, err := url.Parse(c.BrokerURL); err != nil || u.Scheme != "amqps" {
			errs = append(errs, errors.New("BROKER_URL must use amqps:// in production"))
		}
	}
	return errs
}

// ValidateAPI checks what only the HTTP API needs.
func (c Config) ValidateAPI() error {
	var errs []error
	if c.JWTPublicKeyPath == "" {
		errs = append(errs, errors.New("JWT_PUBLIC_KEY_PATH is required"))
	}
	if c.JWTAudience == "" || c.JWTIssuer == "" {
		errs = append(errs, errors.New("JWT_AUDIENCE and JWT_ISSUER are required"))
	}
	if c.RateLimitPerUser < 1 || c.RateLimitWrites < 1 || c.RateLimitPerIP < 1 {
		errs = append(errs, errors.New("rate limits must be positive"))
	}
	return errors.Join(errs...)
}

// ValidateWorker checks what only the notification worker needs.
func (c Config) ValidateWorker() error {
	var errs []error
	if c.BrokerURL == "" {
		errs = append(errs, errors.New("BROKER_URL is required"))
	}
	if c.SMTPHost == "" || c.SMTPFrom == "" {
		errs = append(errs, errors.New("SMTP_HOST and SMTP_FROM are required"))
	}
	return errors.Join(errs...)
}

type reader struct {
	lookup func(string) (string, bool)
	errs   []error
}

func (r *reader) str(key, fallback string) string {
	if v, ok := r.lookup(key); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return fallback
}

func (r *reader) integer(key string, fallback int) int {
	raw := r.str(key, "")
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		r.errs = append(r.errs, fmt.Errorf("%s must be an integer", key))
		return fallback
	}
	return n
}

func (r *reader) list(key string) []string {
	var out []string
	for _, part := range strings.Split(r.str(key, ""), ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}
