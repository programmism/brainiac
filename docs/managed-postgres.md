# Using a managed / external Postgres (#253)

The bundled `db` container (pgvector) is the zero-config default. To scale the data
tier — HA, managed backups, bigger storage — point Brainiac at **RDS / Cloud SQL /
Neon / your own Postgres** instead. Brainiac needs only a standard Postgres **16+
with the `vector` (pgvector) extension**; migrations run themselves on boot.

## 1. Provision the database
- **pgvector must be available.** RDS/Aurora Postgres and Neon ship it; on Cloud SQL
  enable the extension. Brainiac's migrations run `CREATE EXTENSION IF NOT EXISTS
  vector` — the DB role needs permission to create it (or a superuser/admin creates
  it once).
- Create a database and a least-privilege role that owns the schema.

## 2. Point Brainiac at it — with TLS
Set a full `DATABASE_URL` in `.env` and apply the override, which disables the
bundled `db`/`backup` services and reads the DSN from the environment:

```bash
# .env
DATABASE_URL=postgres://USER:PASSWORD@your-instance.rds.amazonaws.com:5432/brainiac?sslmode=require

docker compose -f docker-compose.yml -f docker-compose.managed.yml up -d
```

**Require TLS in the DSN** (never `sslmode=disable` off-box — the default compose
uses it only because the DB is on the private compose network):
- `sslmode=require` — encrypt, but don't verify the server cert (minimum for prod).
- `sslmode=verify-full` — encrypt **and** verify hostname + CA; pass the provider's
  CA via `sslrootcert=/path/to/ca.pem` (mount it into the `app` container). This is
  the recommended setting — it defeats MITM.

## 3. Size the connection pool
Brainiac uses a `pgxpool` connection pool. It's **fully defaulted** (≈ `max(4,
NumCPU)` connections) unless you set it — and pgx reads pool settings straight from
the DSN, so no rebuild is needed:

```
...&pool_max_conns=10&pool_min_conns=2&pool_max_conn_lifetime=30m&pool_health_check_period=1m
```

Sizing rules:
- Keep **`pool_max_conns` × (number of app/MCP processes)** comfortably **below the
  instance's `max_connections`**, leaving headroom for migrations, backups, and your
  own psql sessions. Small managed tiers cap `max_connections` low (e.g. ~100).
- The **MCP server runs one pool per process** (`docker compose exec app
  /brainiac-mcp`). Many concurrent MCP clients → many pools → put a **PgBouncer**
  (transaction pooling) in front and point `DATABASE_URL` at it.
- More connections isn't faster past the point where the DB's CPU/IO saturates —
  size to the workload, not the max.

## 4. Backups
The managed provider does automated backups / PITR — you can **skip** the bundled
`--profile backup` cron (the override disables it). Keep the portable
`brainiac export` (namespace JSON) for cross-instance moves. For self-hosted
Postgres, see [operations.md](operations.md) (pg_dump) and enable WAL archiving for
PITR.

## Notes
- To temporarily run the bundled `db` again (e.g. local testing) without editing
  files, request its profile: `docker compose --profile bundled-db-disabled up db`.
- Everything else is unchanged — `./brainiac`, the WebUI, MCP, and migrations all
  work identically against the managed instance.
