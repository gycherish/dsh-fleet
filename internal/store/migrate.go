// Package store owns the PostgreSQL connection and the forward-only migration
// runner.
package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Open connects to PostgreSQL and verifies the connection before returning.
//
// Verifying here rather than lazily means `dshf serve` fails at boot on a bad
// DSN instead of on the first browser request.
func Open(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("store: cannot configure pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: cannot reach database: %w", err)
	}
	return pool, nil
}

// Migrate applies every unapplied migration in dir, in filename order.
//
// Each file runs inside its own transaction and records its version in
// schema_migrations, so a partial run leaves the database at a known version
// rather than half-way through one file. There are no down migrations: a
// control plane that has already issued node tokens cannot roll its schema
// back in any meaningful sense.
func Migrate(ctx context.Context, pool *pgxpool.Pool, dir string) ([]string, error) {
	files, err := collect(dir)
	if err != nil {
		return nil, err
	}

	applied, err := appliedVersions(ctx, pool)
	if err != nil {
		return nil, err
	}

	var ran []string
	for _, file := range files {
		version := strings.TrimSuffix(filepath.Base(file), ".sql")
		if _, done := applied[version]; done {
			continue
		}
		sql, err := os.ReadFile(file)
		if err != nil {
			return ran, fmt.Errorf("store: cannot read migration %s: %w", version, err)
		}
		if err := runOne(ctx, pool, version, string(sql)); err != nil {
			return ran, err
		}
		ran = append(ran, version)
	}
	return ran, nil
}

// appliedVersions reads schema_migrations, treating its absence as an empty
// database rather than an error: that is exactly the state of a fresh volume.
func appliedVersions(ctx context.Context, pool *pgxpool.Pool) (map[string]struct{}, error) {
	const q = `SELECT version FROM schema_migrations`
	rows, err := pool.Query(ctx, q)
	if err != nil {
		if isUndefinedTable(err) {
			return map[string]struct{}{}, nil
		}
		return nil, fmt.Errorf("store: cannot read schema_migrations: %w", err)
	}
	defer rows.Close()

	applied := map[string]struct{}{}
	for rows.Next() {
		var version string
		if err := rows.Scan(&version); err != nil {
			return nil, fmt.Errorf("store: cannot scan schema_migrations: %w", err)
		}
		applied[version] = struct{}{}
	}
	return applied, rows.Err()
}

func runOne(ctx context.Context, pool *pgxpool.Pool, version, sql string) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: cannot begin migration %s: %w", version, err)
	}
	// Rollback after a successful commit is a no-op, so this covers every path
	// out of the function without a success flag.
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, sql); err != nil {
		return fmt.Errorf("store: migration %s failed: %w", version, err)
	}
	// 0001 records its own version inside the file, since it creates the table.
	// Later migrations rely on this upsert instead of repeating the insert.
	const record = `INSERT INTO schema_migrations (version) VALUES ($1) ON CONFLICT DO NOTHING`
	if _, err := tx.Exec(ctx, record, version); err != nil {
		return fmt.Errorf("store: cannot record migration %s: %w", version, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: cannot commit migration %s: %w", version, err)
	}
	return nil
}

func collect(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("store: cannot read migrations directory %q: %w", dir, err)
	}
	var files []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		files = append(files, filepath.Join(dir, entry.Name()))
	}
	sort.Strings(files)
	if len(files) == 0 {
		return nil, fmt.Errorf("store: no .sql migrations found in %q", dir)
	}
	return files, nil
}

// undefinedTable is the PostgreSQL SQLSTATE for "relation does not exist",
// which is exactly how a first-ever boot presents.
const undefinedTable = "42P01"

// isUndefinedTable reports whether err is the missing-relation error.
func isUndefinedTable(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == undefinedTable
}
