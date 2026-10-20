// Package config decides which database vibe-agent is allowed to talk to.
//
// The tool is local-only by construction: it reads exactly one variable
// (VIBE_AGENT_DATABASE_URL), it never falls back to the ThamBandha API's
// DATABASE_URL, it requires the server to be on the loopback interface, and it
// refuses the product database names outright. There is no override for any of
// those rules, so a copy of this tool on a machine with production credentials
// still cannot point itself at production by accident.
package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

const EnvDatabaseURL = "VIBE_AGENT_DATABASE_URL"

// ErrConfig marks a bad or missing configuration, so the CLI can exit with a
// usage status instead of "database unavailable".
var ErrConfig = errors.New("configuration")

type Error struct{ message string }

func (e *Error) Error() string { return e.message }

func (e *Error) Is(target error) bool { return target == ErrConfig }

func configErrorf(format string, args ...any) error {
	return &Error{message: fmt.Sprintf(format, args...)}
}

var productDatabases = map[string]bool{
	"thambandha":      true,
	"thambandha_api":  true,
	"thambandha_test": true,
}

type Config struct {
	DatabaseURL string
	Database    string
	Host        string
}

// Load reads the tool's database URL from the environment. An unset variable is
// an error rather than a default: a default would be a DSN someone else chose.
func Load() (Config, error) {
	raw := strings.TrimSpace(os.Getenv(EnvDatabaseURL))
	if raw == "" {
		return Config{}, configErrorf(
			"%s is not set; point it at a local database, for example:\n"+
				"  %s='postgres://postgres:postgres@127.0.0.1:5432/vibe_agent?sslmode=disable'",
			EnvDatabaseURL, EnvDatabaseURL)
	}
	return Parse(raw)
}

func Parse(raw string) (Config, error) {
	cfg, err := pgxpool.ParseConfig(strings.TrimSpace(raw))
	if err != nil {
		return Config{}, configErrorf("%s is not a valid connection string: %v", EnvDatabaseURL, err)
	}

	host := cfg.ConnConfig.Host
	if !localHost(host) {
		return Config{}, configErrorf(
			"refusing to connect to %q: %s exists for local development only and the database "+
				"must be on this machine (localhost, 127.0.0.1 or ::1)",
			host, EnvDatabaseURL)
	}

	database := cfg.ConnConfig.Database
	if productDatabases[strings.ToLower(database)] {
		return Config{}, configErrorf(
			"refusing to use the ThamBandha database %q: vibe-agent keeps its history in its own "+
				"database, for example `vibe_agent`",
			database)
	}
	if database == "" {
		return Config{}, configErrorf("the connection string does not name a database")
	}

	return Config{
		DatabaseURL: raw,
		Database:    database,
		Host:        host,
	}, nil
}

func localHost(host string) bool {
	host = strings.TrimSpace(host)
	if host == "" || strings.EqualFold(host, "localhost") {
		return true
	}
	// A unix socket is expressed as a filesystem path.
	if strings.HasPrefix(host, "/") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}
