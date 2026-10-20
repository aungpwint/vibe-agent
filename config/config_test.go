package config_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/aungpwint/vibe-agent/config"
)

func TestParseAcceptsLoopbackHosts(t *testing.T) {
	for _, dsn := range []string{
		"postgres://postgres:postgres@localhost:5432/vibe_agent",
		"postgres://postgres:postgres@127.0.0.1:5432/vibe_agent",
		"postgres://postgres:postgres@[::1]:5432/vibe_agent",
		"postgres:///vibe_agent",
	} {
		cfg, err := config.Parse(dsn)
		if err != nil {
			t.Errorf("Parse(%q) = %v, want acceptance", dsn, err)
			continue
		}
		if cfg.Database != "vibe_agent" {
			t.Errorf("Parse(%q) database = %q", dsn, cfg.Database)
		}
	}
}

func TestParseRejectsRemoteHosts(t *testing.T) {
	for _, dsn := range []string{
		"postgres://postgres:postgres@10.20.30.40:5432/vibe_agent",
		"postgres://postgres:postgres@db.example.com:5432/vibe_agent",
	} {
		_, err := config.Parse(dsn)
		if err == nil {
			t.Errorf("Parse(%q) accepted a remote host", dsn)
			continue
		}
		if !errors.Is(err, config.ErrConfig) {
			t.Errorf("Parse(%q) error = %v, want config.ErrConfig", dsn, err)
		}
		if !strings.Contains(err.Error(), "local") {
			t.Errorf("Parse(%q) error = %v, want a local-only explanation", dsn, err)
		}
	}
}

func TestParseRejectsProductDatabases(t *testing.T) {
	for _, database := range []string{"thambandha", "thambandha_api", "thambandha_test", "ThamBandha_API"} {
		dsn := "postgres://postgres:postgres@127.0.0.1:5432/" + database
		_, err := config.Parse(dsn)
		if err == nil {
			t.Errorf("Parse(%q) accepted a product database", dsn)
			continue
		}
		if !errors.Is(err, config.ErrConfig) {
			t.Errorf("Parse(%q) error = %v, want config.ErrConfig", dsn, err)
		}
		if !strings.Contains(err.Error(), "refusing") {
			t.Errorf("Parse(%q) error = %v, want an explicit refusal", dsn, err)
		}
	}
}

func TestParseRejectsInvalidDSN(t *testing.T) {
	_, err := config.Parse("://not-a-dsn")
	if err == nil {
		t.Fatal("Parse accepted an invalid DSN")
	}
	if !errors.Is(err, config.ErrConfig) {
		t.Errorf("Parse(invalid) error = %v, want config.ErrConfig", err)
	}
}

func TestLoadRequiresItsOwnVariableAndIgnoresDATABASE_URL(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://postgres:postgres@127.0.0.1:5432/vibe_agent")
	t.Setenv(config.EnvDatabaseURL, "")

	_, err := config.Load()
	if err == nil {
		t.Fatal("Load succeeded without its own variable")
	}
	if !errors.Is(err, config.ErrConfig) {
		t.Errorf("Load error = %v, want config.ErrConfig", err)
	}
	if !strings.Contains(err.Error(), config.EnvDatabaseURL) {
		t.Errorf("Load error = %v, want it to name %s", err, config.EnvDatabaseURL)
	}
	if !strings.Contains(err.Error(), "not set") {
		t.Errorf("Load error = %v, want the unset-variable explanation", err)
	}
}
