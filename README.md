# Brainiac

A **self-hosted, general-purpose memory platform**. It remembers not just *what* exists but **why it is
this way** — decisions, trade-offs, rejected alternatives, who and when.

- **Semantic search** over a curated corpus (Postgres + pgvector, local Ollama embeddings).
- **Curated knowledge graph** where every edge carries a `why`, its provenance, and its author.
- **Captured through chat with Claude** — finish an investigation, tell Claude "save this," and it lands
  in the base with its associations. No expensive extraction LLM, no cloud-LLM bill.
- **Core + plugins + clients:** one core holds all logic; plugins (connectors/extractors/selectors/
  embedders) make it domain-agnostic; thin clients (Claude/MCP, WebUI, CLI) just call the core.

> 📖 **[SYSTEM.md](SYSTEM.md)** is the living spec — architecture, technology decisions and their
> rationale, data model, and the decision log. Read it before contributing.

## Quickstart

```bash
git clone https://github.com/programmism/brainiac
cd brainiac
cp .env.example .env      # sane defaults; set your secrets
docker compose up         # → db (pgvector) + ollama + app, migrations + model pulled automatically
```

Easy deployment is a hard requirement: one command yields a healthy stack, on a 4 GB prototype box.
The app **verifies its own state** — `GET /healthz` (liveness) and `GET /readyz` (readiness: DB-gated,
embedder reported), and a CI smoke test boots the stack and asserts readiness end-to-end.

### Updating

The `app` service is **built from this checkout**, so updating is: get the new code, then rebuild.

```bash
cd brainiac
git pull                       # latest main — or `git fetch --tags && git checkout v1.16.0` to pin a release
docker compose up -d --build   # rebuilds & recreates `app`; only what changed is touched
```

- **Migrations apply automatically** on `app` boot (idempotent) — no manual step.
- **Your data is safe:** the corpus and models live in the `pgdata` / `ollama` named volumes, which a
  rebuild never touches. (`docker compose down` alone also keeps them; only `down -v` deletes volumes.)
- **Verify:** `curl -s localhost:8080/readyz` → `{"db":"ok",...}`, then open the WebUI **System** tab (or
  `curl -s localhost:8080/api/system`) to confirm the new version is live and healthy.
- **Roll back** the same way: `git checkout <previous tag> && docker compose up -d --build`.
- Note: the MCP server runs *inside* `app` (`./brainiac mcp-config`), so recreating `app` briefly drops
  the MCP connection — your agent reconnects on its next call.

**👉 Using it on your laptop** — `./brainiac up`, drop Markdown into `./data/docs` (auto-imported),
`./brainiac search "…"`, and `./brainiac mcp-config` to connect Claude. No Go, no exposed ports.
See [docs/laptop.md](docs/laptop.md).

**👉 Making it an agent's memory** — connect any MCP agent (Claude, Cursor, Cline, or your own),
then give it the memory instruction (`./brainiac instructions`) so it recalls before answering and
saves findings/decisions on its own — globally or per-project. See [docs/agent-memory.md](docs/agent-memory.md).

## Status

**M0–M4 complete — the full roadmap is done; usable as a knowledge base today.** capture→recall core
(MCP + CLI), ingestion + density selection, Notion **and** Markdown connectors (plugin seams frozen),
read-only + interactive WebUI (search / recall / consolidation queue / graph / health / system), the librarian
consolidation pass (CLI + WebUI + cron), reverse proxy + auth (Caddy), daily backups, recall@k eval, and
per-project identity scoping. Currently **hardening for real production use (M5)** — see
[docs/production-readiness.md](docs/production-readiness.md).

Runs on a 4 GB prototype box today; size up before real production load. See
[SYSTEM.md §10](SYSTEM.md#10-decision-log) for the full decision log.

## Stack

Go 1.25+ · Postgres 16 + pgvector · Ollama (`nomic-embed-text`) · net/http + chi · pgx · cobra · MCP ·
Docker Compose · Caddy. See [SYSTEM.md §3](SYSTEM.md#3-technology-decisions-and-why) for the rationale
behind each choice.

## License

MIT — see [LICENSE](LICENSE).
