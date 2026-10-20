package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// CreateDatabase creates the local database named in the DSN if it does not
// exist yet. It connects to the server's maintenance database (postgres, then
// template1) rather than to the target, because a database that does not exist
// cannot be connected to. Returns created=false when it already existed.
func CreateDatabase(ctx context.Context, dsn string) (bool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return false, fmt.Errorf("store: the database URL is not a valid connection string: %w", err)
	}
	database := cfg.ConnConfig.Database
	if database == "" {
		return false, errors.New("store: the database URL does not name a database")
	}

	var lastErr error
	for _, maintenance := range []string{"postgres", "template1"} {
		attempt := cfg.Copy()
		attempt.ConnConfig.Database = maintenance
		attempt.MaxConns = 1
		attempt.MinConns = 1

		conn, err := pgxpool.NewWithConfig(ctx, attempt)
		if err != nil {
			lastErr = err
			continue
		}

		var exists bool
		err = conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, database).Scan(&exists)
		if err != nil {
			conn.Close()
			lastErr = err
			continue
		}
		if exists {
			conn.Close()
			return false, nil
		}

		quoted := pgx.Identifier{database}.Sanitize()
		_, err = conn.Exec(ctx, "CREATE DATABASE "+quoted)
		conn.Close()
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "42P04" {
				return false, nil
			}
			return false, fmt.Errorf("store: cannot create the database %q: %w", database, err)
		}
		return true, nil
	}
	return false, fmt.Errorf("store: cannot reach the server to create %q: %w", database, lastErr)
}
