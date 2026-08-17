// Package config loads the control plane's runtime configuration.
//
// Every value comes from the environment. The Docker path is the supported
// deployment, and compose already owns an env file, so a second configuration
// format would only be a second place for the two to disagree.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
)

// Config is the validated runtime configuration.
type Config struct {
	// DatabaseURL is a libpq connection string.
	DatabaseURL string
	// Listen is the HTTP bind address, e.g. ":8080".
	Listen string
	// PublicURL is the absolute origin browsers use. Cookie scope and the
	// uplink URL printed by `dshf node add` derive from it.
	PublicURL *url.URL
	// MigrationsDir holds the forward-only SQL migrations.
	MigrationsDir string
	// AdminUser and AdminPassword bootstrap the first account. They are used
	// only while the users table is empty.
	AdminUser     string
	AdminPassword string
	// TLSCert and TLSKey enable HTTPS when both are set.
	//
	// This is closer to required than optional. Browsers gate
	// `crypto.randomUUID` and friends behind a secure context, and only HTTPS
	// and loopback qualify — so a control plane reached at a LAN address over
	// plain HTTP serves a dsh UI whose settings pages fail outright.
	TLSCert string
	TLSKey  string
	// PrivilegedAccess is how much of dsh's loopback-pinned method set the
	// browser plane may reach: none, read, or full. Defaults to full, so the
	// console can actually drive the machine; narrow it for a read-only one.
	PrivilegedAccess string
	// LogLevel is one of debug, info, warn, error.
	LogLevel string
}

// ServesTLS reports whether both halves of a certificate pair are configured.
func (c *Config) ServesTLS() bool { return c.TLSCert != "" && c.TLSKey != "" }

// ErrMissing reports a required variable that was unset or blank.
var ErrMissing = errors.New("config: required environment variable is not set")

// Load reads and validates the configuration from the process environment.
//
// It fails loud rather than defaulting anything security-relevant: a control
// plane that silently invented an admin password or a public origin would be
// worse than one that refuses to start.
func Load() (*Config, error) {
	cfg := &Config{
		DatabaseURL:      os.Getenv("DSHF_DATABASE_URL"),
		Listen:           envOr("DSHF_LISTEN", ":8080"),
		MigrationsDir:    envOr("DSHF_MIGRATIONS_DIR", "deploy/migrations"),
		AdminUser:        envOr("DSHF_ADMIN_USER", "admin"),
		AdminPassword:    os.Getenv("DSHF_ADMIN_PASSWORD"),
		TLSCert:          os.Getenv("DSHF_TLS_CERT"),
		TLSKey:           os.Getenv("DSHF_TLS_KEY"),
		PrivilegedAccess: envOr("DSHF_PRIVILEGED_ACCESS", "full"),
		LogLevel:         envOr("DSHF_LOG_LEVEL", "info"),
	}

	if (cfg.TLSCert == "") != (cfg.TLSKey == "") {
		return nil, errors.New("config: set both DSHF_TLS_CERT and DSHF_TLS_KEY, or neither")
	}

	if strings.TrimSpace(cfg.DatabaseURL) == "" {
		return nil, fmt.Errorf("%w: DSHF_DATABASE_URL", ErrMissing)
	}
	if strings.TrimSpace(cfg.AdminPassword) == "" {
		return nil, fmt.Errorf("%w: DSHF_ADMIN_PASSWORD", ErrMissing)
	}

	raw := envOr("DSHF_PUBLIC_URL", "")
	if strings.TrimSpace(raw) == "" {
		return nil, fmt.Errorf("%w: DSHF_PUBLIC_URL", ErrMissing)
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("config: DSHF_PUBLIC_URL is not a valid URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("config: DSHF_PUBLIC_URL must be http or https, got %q", parsed.Scheme)
	}
	if parsed.Host == "" {
		return nil, errors.New("config: DSHF_PUBLIC_URL must include a host")
	}
	cfg.PublicURL = parsed

	return cfg, nil
}

// LoadDatabaseURL reads only the connection string.
//
// The operator subcommands touch the database but serve no traffic, so
// demanding a public URL and an admin password from them would make `dshf node
// add` fail for reasons that have nothing to do with adding a node.
func LoadDatabaseURL() (string, error) {
	dsn := os.Getenv("DSHF_DATABASE_URL")
	if strings.TrimSpace(dsn) == "" {
		return "", fmt.Errorf("%w: DSHF_DATABASE_URL", ErrMissing)
	}
	return dsn, nil
}

func envOr(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok && value != "" {
		return value
	}
	return fallback
}
