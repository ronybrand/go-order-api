package config

import (
	"strings"
	"testing"
)

func env(values map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) { v, ok := values[key]; return v, ok }
}

func TestLoad_DevelopmentDefaults(t *testing.T) {
	cfg, err := load(env(map[string]string{"DATABASE_URL": "postgres://x"}))

	if err != nil {
		t.Fatal(err)
	}
	if cfg.IsProduction() || cfg.HTTPAddr != ":8080" || cfg.MaxBodyBytes != 1<<20 || cfg.RateLimitPerUser != 120 {
		t.Errorf("unexpected defaults: %+v", cfg)
	}
}

func TestLoad_ProductionRules(t *testing.T) {
	base := map[string]string{
		"APP_ENV": "production", "DATABASE_URL": "postgres://x", "JWT_AUDIENCE": "a", "JWT_ISSUER": "i",
		"CORS_ORIGINS": "https://app.example", "BROKER_URL": "amqps://broker",
	}
	with := func(key, value string) map[string]string {
		m := map[string]string{}
		for k, v := range base {
			m[k] = v
		}
		m[key] = value
		return m
	}

	tests := map[string]struct {
		env     map[string]string
		wantErr string
	}{
		"valid":                {env: base},
		"no audience":          {env: with("JWT_AUDIENCE", ""), wantErr: "JWT_AUDIENCE"},
		"no issuer":            {env: with("JWT_ISSUER", ""), wantErr: "JWT_ISSUER"},
		"no cors origins":      {env: with("CORS_ORIGINS", ""), wantErr: "CORS_ORIGINS"},
		"wildcard cors":        {env: with("CORS_ORIGINS", "*"), wantErr: "CORS_ORIGINS"},
		"plain amqp broker":    {env: with("BROKER_URL", "amqp://broker"), wantErr: "amqps"},
		"unknown environment":  {env: with("APP_ENV", "staging"), wantErr: "APP_ENV"},
		"malformed integer":    {env: with("SMTP_PORT", "abc"), wantErr: "SMTP_PORT"},
		"missing database url": {env: with("DATABASE_URL", ""), wantErr: "DATABASE_URL"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := load(env(tt.env))

			switch {
			case tt.wantErr == "" && err != nil:
				t.Errorf("unexpected error: %v", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Errorf("err = %v, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoad_ParsesCORSOriginList(t *testing.T) {
	cfg, err := load(env(map[string]string{"DATABASE_URL": "x", "CORS_ORIGINS": " https://a.example , https://b.example,"}))

	if err != nil || len(cfg.CORSOrigins) != 2 || cfg.CORSOrigins[1] != "https://b.example" {
		t.Errorf("origins = %v, err = %v", cfg.CORSOrigins, err)
	}
}

func TestValidateAPIAndWorker(t *testing.T) {
	cfg := Config{}

	if cfg.ValidateAPI() == nil || cfg.ValidateWorker() == nil {
		t.Error("an empty config must fail both validations")
	}
}
