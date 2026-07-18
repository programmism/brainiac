package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ReencryptAll re-encrypts every encrypted field under the current primary key
// (#403) — the active half of key rotation: after setting a new ENCRYPTION_KEY (with
// the old one listed as retired so reads still work), `kb reencrypt` migrates chunk
// text, edge why, and node summary/rollup off the old key so it can be dropped.
// Values already under the primary (or plaintext with encryption off) are skipped.
// Returns how many values were rewritten. No-op error if no key is configured.
func ReencryptAll(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	if cipherPrimaryID == "" {
		return 0, fmt.Errorf("no ENCRYPTION_KEY configured — nothing to re-encrypt")
	}
	total := 0
	for _, tc := range []struct{ table, col string }{
		{"chunks", "text"},
		{"edges", "why"},
		{"nodes", "summary"},
		{"nodes", "rollup"},
	} {
		n, err := reencryptColumn(ctx, pool, tc.table, tc.col)
		if err != nil {
			return total, fmt.Errorf("re-encrypt %s.%s: %w", tc.table, tc.col, err)
		}
		total += n
	}
	return total, nil
}

// reencryptColumn rewrites one text column's encrypted values under the primary key.
// table/col are fixed internal identifiers (never user input).
func reencryptColumn(ctx context.Context, pool *pgxpool.Pool, table, col string) (int, error) {
	rows, err := pool.Query(ctx, fmt.Sprintf(`SELECT id, %s FROM %s WHERE %s IS NOT NULL`, col, table, col))
	if err != nil {
		return 0, err
	}
	type row struct {
		id  string
		val string
	}
	var todo []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.val); err != nil {
			rows.Close()
			return 0, err
		}
		todo = append(todo, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	n := 0
	update := fmt.Sprintf(`UPDATE %s SET %s = $2 WHERE id = $1`, table, col)
	for _, r := range todo {
		next, changed, err := reencryptValue(r.val)
		if err != nil {
			return n, err
		}
		if !changed {
			continue
		}
		if _, err := pool.Exec(ctx, update, r.id, next); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
