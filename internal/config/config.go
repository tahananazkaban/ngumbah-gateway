// Package config loads gateway settings from environment variables.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"
)

// DefaultHTTPAddr is the listen address when GATEWAY_HTTP_ADDR is unset.
const DefaultHTTPAddr = ":8080"

// HTTPAddr returns the configured listen address without validating anything else.
func HTTPAddr() string { return getenv("GATEWAY_HTTP_ADDR", DefaultHTTPAddr) }

// Config holds every runtime setting of the gateway.
type Config struct {
	HTTPAddr      string
	TenantSvcAddr string
	CORSOrigins   []string
	Env           string
	LogLevel      slog.Level
	TraceExporter string
}

// Development reports whether development-only features (playground, introspection) are on.
func (c Config) Development() bool { return c.Env == "development" }

// Load reads the environment and validates it; all problems are reported at once.
func Load() (Config, error) {
	cfg := Config{
		HTTPAddr:      HTTPAddr(),
		TenantSvcAddr: getenv("GATEWAY_TENANT_SVC_ADDR", "localhost:50051"),
		CORSOrigins:   splitList(getenv("GATEWAY_CORS_ORIGINS", "http://localhost:5173")),
		Env:           getenv("GATEWAY_ENV", "production"),
		TraceExporter: getenv("OTEL_TRACES_EXPORTER", "none"),
	}

	var errs []error
	if cfg.Env != "development" && cfg.Env != "production" {
		errs = append(errs, fmt.Errorf("GATEWAY_ENV must be development or production, got %q", cfg.Env))
	}
	if err := cfg.LogLevel.UnmarshalText([]byte(getenv("GATEWAY_LOG_LEVEL", "info"))); err != nil {
		errs = append(errs, fmt.Errorf("GATEWAY_LOG_LEVEL: %w", err))
	}
	if len(cfg.CORSOrigins) == 0 {
		errs = append(errs, errors.New("GATEWAY_CORS_ORIGINS must list at least one origin"))
	}
	if _, _, err := net.SplitHostPort(cfg.HTTPAddr); err != nil {
		errs = append(errs, fmt.Errorf("GATEWAY_HTTP_ADDR: %w", err))
	}
	if _, _, err := net.SplitHostPort(cfg.TenantSvcAddr); err != nil {
		errs = append(errs, fmt.Errorf("GATEWAY_TENANT_SVC_ADDR: %w", err))
	}
	for _, o := range cfg.CORSOrigins {
		if o == "*" {
			errs = append(errs, errors.New("GATEWAY_CORS_ORIGINS must list explicit origins, not *"))
		}
	}
	if cfg.TraceExporter != "none" {
		errs = append(errs, fmt.Errorf("OTEL_TRACES_EXPORTER %q is not supported yet; use none", cfg.TraceExporter))
	}
	return cfg, errors.Join(errs...)
}

func splitList(v string) []string {
	var out []string
	for _, part := range strings.Split(v, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func getenv(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}
