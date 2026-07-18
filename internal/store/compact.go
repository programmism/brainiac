package store

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

// VacuumAnalyze reclaims the dead-tuple bloat left by superseded/deleted rows
// (supersession never deletes, retention/dedup do) and refreshes planner
// statistics on the main tables (#385). It's a plain VACUUM — non-blocking, reads
// and writes continue — not VACUUM FULL. VACUUM cannot run inside a transaction, so
// it uses the simple query protocol on a dedicated connection (pgx's default
// extended protocol would wrap it in an implicit transaction and fail).
func VacuumAnalyze(ctx context.Context, pool *pgxpool.Pool) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	_, err = conn.Conn().PgConn().Exec(ctx, `VACUUM (ANALYZE) chunks, chunk_sources, nodes, edges`).ReadAll()
	return err
}
