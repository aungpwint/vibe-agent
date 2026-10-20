package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/aungpwint/vibe-agent/migrations"
)

type Migration struct {
	Version  string
	Name     string
	SQL      string
	Checksum string
}

type MigrationStatus struct {
	Version   string
	Name      string
	Checksum  string
	AppliedAt time.Time
}

// normaliseNewlines makes a CRLF checkout hash the same as an LF one, so a
// migration that moved between Windows and Unix still verifies.
func normaliseNewlines(raw []byte) []byte {
	return bytes.ReplaceAll(raw, []byte("\r\n"), []byte("\n"))
}

func checksum(raw []byte) string {
	sum := sha256.Sum256(normaliseNewlines(raw))
	return hex.EncodeToString(sum[:])
}

func Load() ([]Migration, error) {
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		return nil, fmt.Errorf("store: cannot read the embedded migrations: %w", err)
	}

	out := make([]Migration, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		version, name, ok := parseName(entry.Name())
		if !ok {
			return nil, fmt.Errorf("store: migration %q is not named NNNN_name.sql", entry.Name())
		}
		raw, err := fs.ReadFile(migrations.FS, entry.Name())
		if err != nil {
			return nil, fmt.Errorf("store: cannot read %q: %w", entry.Name(), err)
		}
		out = append(out, Migration{
			Version:  version,
			Name:     name,
			SQL:      string(normaliseNewlines(raw)),
			Checksum: checksum(raw),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })

	for i := 1; i < len(out); i++ {
		if out[i].Version == out[i-1].Version {
			return nil, fmt.Errorf("store: two migrations share version %s", out[i].Version)
		}
	}
	return out, nil
}

func parseName(filename string) (version, name string, ok bool) {
	if !strings.HasSuffix(filename, ".sql") {
		return "", "", false
	}
	base := strings.TrimSuffix(filename, ".sql")
	if len(base) < 6 || base[4] != '_' {
		return "", "", false
	}
	version = base[:4]
	for _, r := range version {
		if r < '0' || r > '9' {
			return "", "", false
		}
	}
	name = base[5:]
	if name == "" {
		return "", "", false
	}
	return version, name, true
}

// Migrate applies every embedded migration the ledger does not know yet. A file
// whose checksum drifted after it was applied is a hard error: editing an
// applied migration silently desynchronises every existing database.
func Migrate(ctx context.Context, pool *Pool) ([]string, error) {
	const ledger = `
    CREATE TABLE IF NOT EXISTS vibe_schema_migrations (
        version    TEXT        PRIMARY KEY,
        name       TEXT        NOT NULL,
        checksum   TEXT        NOT NULL,
        applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
    )`

	if _, err := pool.Exec(ctx, ledger); err != nil {
		return nil, fmt.Errorf("store: cannot create the migration ledger: %w", err)
	}

	all, err := Load()
	if err != nil {
		return nil, err
	}

	rows, err := pool.Query(ctx, `SELECT version, checksum FROM vibe_schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("store: cannot read the migration ledger: %w", err)
	}
	applied := make(map[string]string)
	for rows.Next() {
		var version, sum string
		if err := rows.Scan(&version, &sum); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: cannot read the migration ledger: %w", err)
		}
		applied[version] = sum
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: cannot read the migration ledger: %w", err)
	}

	var ran []string
	for _, m := range all {
		if sum, ok := applied[m.Version]; ok {
			if sum != m.Checksum {
				return nil, fmt.Errorf(
					"store: migration %s_%s changed after it was applied; migrations are immutable",
					m.Version, m.Name)
			}
			continue
		}

		err := pool.InTx(ctx, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, m.SQL); err != nil {
				return err
			}
			const record = `
    INSERT INTO vibe_schema_migrations (version, name, checksum) VALUES ($1, $2, $3)`
			_, err := tx.Exec(ctx, record, m.Version, m.Name, m.Checksum)
			return err
		})
		if err != nil {
			return nil, fmt.Errorf("store: migration %s_%s failed: %w", m.Version, m.Name, err)
		}
		ran = append(ran, m.Version+"_"+m.Name)
	}
	return ran, nil
}

func Status(ctx context.Context, pool *Pool) ([]MigrationStatus, error) {
	const query = `
    SELECT version, name, checksum, applied_at
      FROM vibe_schema_migrations
     ORDER BY version`

	rows, err := pool.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("store: cannot read the migration ledger: %w", err)
	}
	defer rows.Close()

	out := make([]MigrationStatus, 0, 4)
	for rows.Next() {
		var s MigrationStatus
		if err := rows.Scan(&s.Version, &s.Name, &s.Checksum, &s.AppliedAt); err != nil {
			return nil, fmt.Errorf("store: cannot read the migration ledger: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Pending reports which embedded migrations have not been applied yet.
func Pending(ctx context.Context, pool *Pool) ([]Migration, error) {
	all, err := Load()
	if err != nil {
		return nil, err
	}

	applied := make(map[string]bool)
	status, err := Status(ctx, pool)
	if err != nil {
		// An un-migrated database has no ledger table yet; every file is pending.
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42P01" {
			return nil, err
		}
	} else {
		for _, s := range status {
			applied[s.Version] = true
		}
	}

	out := make([]Migration, 0, len(all))
	for _, m := range all {
		if !applied[m.Version] {
			out = append(out, m)
		}
	}
	return out, nil
}
