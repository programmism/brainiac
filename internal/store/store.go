// Package store owns all database access: the connection pool, the migration
// runner, and (as it grows) the repository layer that is the only place SQL
// lives. See SYSTEM.md §3 and §5.
package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// hnswEFSearch is the HNSW candidate-list size for vector queries. The pgvector
// default (40) is too small once reads are filtered by scope/wall: the ANN scan
// returns its k-nearest and THEN the filter drops out-of-scope rows, so a scoped
// query can come back with far fewer than k results — a silent recall cliff for
// multi-tenant isolation. A larger list widens the candidate pool before filtering
// (#212). iterative_scan (pgvector 0.8+) additionally keeps scanning until enough
// in-filter rows are found; it's set best-effort so older pgvector still works.
const hnswEFSearch = 100

// Connect opens a pgx connection pool to dsn and verifies it with a ping. Each
// connection is tuned for filtered vector search (see hnswEFSearch).
func Connect(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		// Best-effort HNSW tuning: never fail a connection over it (the query still
		// works at the pgvector default). Postgres accepts these as placeholders
		// even before the extension is loaded on a fresh DB, and pgvector adopts
		// them once loaded; iterative_scan needs pgvector 0.8+.
		_, _ = conn.Exec(ctx, fmt.Sprintf("SET hnsw.ef_search = %d", hnswEFSearch))
		_, _ = conn.Exec(ctx, "SET hnsw.iterative_scan = relaxed_order")
		return nil
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}

// ConnectWithRetry retries Connect with exponential backoff until it succeeds or
// maxWait elapses — so a service booting before Postgres is ready waits instead
// of crash-looping (#78).
func ConnectWithRetry(ctx context.Context, dsn string, maxWait time.Duration) (*pgxpool.Pool, error) {
	ctx, cancel := context.WithTimeout(ctx, maxWait)
	defer cancel()

	backoff := 500 * time.Millisecond
	for {
		pool, err := Connect(ctx, dsn)
		if err == nil {
			return pool, nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("database not reachable within %s: %w", maxWait, err)
		case <-time.After(backoff):
		}
		if backoff < 5*time.Second {
			backoff *= 2
		}
	}
}
