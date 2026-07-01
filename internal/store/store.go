// Package store owns all database access: the connection pool, the migration
// runner, and (as it grows) the repository layer that is the only place SQL
// lives. See SYSTEM.md §3 and §5.
package store

import (
	"context"

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
