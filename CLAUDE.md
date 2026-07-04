# Brainiac — Project Rules

## System Specification

**Read [SYSTEM.md](SYSTEM.md) before working on any task.** It describes how the system works, the
architecture, technology decisions and *why* we made them, the data model, thresholds, and expected
behavior. Use it to understand context when debugging or implementing.

**Keep SYSTEM.md up to date — in the same PR as the change.** When you add, change, or remove a feature,
update the relevant section and add a dated line to the Decision Log (§10). When fixing a bug, document
the constraint or edge case if not already covered. Code says *what*; SYSTEM.md says *why*.

## Core rule: logic lives in the core

All logic — search, remember, link, recall, supersede, consolidation, ingest — lives in **one core
package**. The MCP server, WebUI, and CLI are **thin adapters** that forward to the core. Never put
business logic in a client; if two clients need the same behavior, it belongs in the core.

## Deployment is a feature

"Very easy to deploy" is a hard requirement. Any change must preserve the one-command
`docker compose up` experience: migrations and the Ollama model pull stay automatic and idempotent; no
manual setup beyond editing `.env`.

## Release Tags

**Tags MUST be semver: `v1.x.x`** (e.g. `v1.2.0`). Never date-based or hash-based.

Find the latest tag, then increment (patch = fix, minor = feature, major = breaking):
```
git tag --sort=-v:refname | grep '^v[0-9]*\.[0-9]' | head -1
```

## Running Tests

Tests are **DB-gated**: anything touching the store/core/server skips unless `DATABASE_URL` is set, and
needs a **Postgres with pgvector**. The tests apply migrations themselves (`store.Migrate`) and `TRUNCATE`
between cases — you only provide an empty database.

**Never point tests at — or otherwise disturb — the live stack.** The running `docker compose` project
(named `brainiac`) is not just the app: **the MCP memory server runs *inside* the `app` container**
(`docker compose exec app /brainiac-mcp`, see `make mcp-config`). So `docker compose down` / `make down`,
or a `docker compose up --build` that recreates `app`, **kills the deployment *and* the MCP server** —
and pointing tests at its DB on `:5432` pollutes real data. Don't do either for testing.

**Correct pattern — a throwaway, isolated Postgres on a spare port** (mirrors CI: `pgvector/pgvector:pg16`,
db `brainiac_test`, user/pass `postgres`):

```bash
docker run -d --rm --name brainiac-test-db \
  -e POSTGRES_USER=postgres -e POSTGRES_PASSWORD=postgres -e POSTGRES_DB=brainiac_test \
  -p 55432:5432 pgvector/pgvector:pg16
until docker exec brainiac-test-db pg_isready -U postgres >/dev/null 2>&1; do sleep 1; done

DATABASE_URL='postgres://postgres:postgres@localhost:55432/brainiac_test?sslmode=disable' go test ./...

docker stop brainiac-test-db   # --rm auto-removes it
```

Notes:
- Use a **non-default host port** (e.g. `55432`) so you never collide with the live stack's `:5432`.
- Run the **whole suite against one fresh DB**; a DB reused across many runs can cause spurious failures
  (e.g. cross-package recall tests seeing leftover chunks). CI is authoritative — it always uses a fresh DB.
- For **deploy validation**, likewise isolate: use a separate compose project
  (`docker compose -p brainiac-validate up -d …`) and tear *that* down — never the default `brainiac` project.

## Git Workflow

- Never push directly to `main`. Always branch + PR.
- One issue = one PR. **Create the GitHub issue BEFORE starting work** so progress is not lost if a
  session hangs.
- Wait for PR checks to pass before merging. Fix red checks; don't merge around them.
- After merge, create a semver tag and validate deployment. Deploy autonomously — don't wait for
  confirmation on merge/tag/deploy.

## Work Tracking

- Work the milestone backlog (M0 → M4) in order; respect stated dependencies (e.g. research spikes
  before the connector/transport they unblock).
- If a task has multiple parts, create multiple issues upfront.
