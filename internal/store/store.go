// Package store owns all database access: the connection pool, the migration
// runner, and (as it grows) the repository layer that is the only place SQL
// lives. See SYSTEM.md §3 and §5.
package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Connect opens a pgx connection pool to dsn and verifies it with a ping.
func Connect(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, dsn)
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
