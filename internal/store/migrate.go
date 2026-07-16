package store

import (
	"context"
	"embed"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// migrateLockKey is the advisory-lock key that serializes Migrate across
// processes (arbitrary constant). Two app instances or an overlapping rolling
// update would otherwise race on schema_migrations / concurrent DDL (#251).
const migrateLockKey int64 = 0x4272_6169_6E61_63 // "Brainac"

// Migrate applies every embedded, not-yet-applied SQL migration in lexical
// order, each wrapped in its own transaction, and records it in
// schema_migrations. It is idempotent and safe to run on every boot; a
// session-level advisory lock serializes concurrent runs across processes.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	// Hold a session-level advisory lock on a dedicated connection for the whole
	// run, so only one process migrates at a time; others block here, then re-check
	// the applied set and no-op.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire migrate connection: %w", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrateLockKey); err != nil {
		return fmt.Errorf("acquire migrate lock: %w", err)
	}
	defer func() { _, _ = conn.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", migrateLockKey) }()

	if err := ensureVersionTable(ctx, pool); err != nil {
		return fmt.Errorf("ensure schema_migrations: %w", err)
	}
	applied, err := appliedVersions(ctx, pool)
	if err != nil {
		return fmt.Errorf("read applied versions: %w", err)
	}

	names, err := migrationNames()
	if err != nil {
		return err
	}
	for _, name := range names {
		if applied[name] {
			continue
		}
		body, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}
		if err := applyOne(ctx, pool, name, string(body)); err != nil {
			return fmt.Errorf("apply %s: %w", name, err)
		}
	}
	return nil
}

func migrationNames() ([]string, error) {
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return nil, fmt.Errorf("list migrations: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

func ensureVersionTable(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    text PRIMARY KEY,
		applied_at timestamptz NOT NULL DEFAULT now()
	)`)
	return err
}

func appliedVersions(ctx context.Context, pool *pgxpool.Pool) (map[string]bool, error) {
	rows, err := pool.Query(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	applied := make(map[string]bool)
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		applied[v] = true
	}
	return applied, rows.Err()
}

// applyOne runs a migration file and its bookkeeping insert as a single atomic
// unit. The simple query protocol executes the multi-statement DDL in one
// server round-trip; the explicit BEGIN/COMMIT makes it all-or-nothing.
func applyOne(ctx context.Context, pool *pgxpool.Pool, name, body string) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	sql := "BEGIN;\n" + body + "\nINSERT INTO schema_migrations (version) VALUES (" + quoteLiteral(name) + ");\nCOMMIT;"
	if _, err := conn.Conn().PgConn().Exec(ctx, sql).ReadAll(); err != nil {
		return err
	}
	return nil
}

func quoteLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
