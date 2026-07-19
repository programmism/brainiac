# Getting started — from zero to full power

One page, simple → advanced. **Golden rule: everything is opt‑in.** The default
config just works for one person locally; every feature below is something you *turn
on* when you need it. Nothing here is required to start.

CLI examples use the `./brainiac` wrapper (see [laptop.md](laptop.md)); it's the same
as `docker compose exec app kb …`.

---

## Level 0 — Run it (required: nothing but this)

```bash
git clone https://github.com/programmism/brainiac && cd brainiac
cp .env.example .env      # defaults are fine
./brainiac up             # first boot downloads a ~270 MB embedding model
./brainiac health         # db ok, embedder ok  → you're live
```

- WebUI: **http://localhost:8080**. Health/ready: `/healthz`, `/readyz`.
- **Add documents:** drop `.md`/`.txt`/`.pdf`/`.docx`/`.html` files into `./data/docs`.
  They're auto‑imported every `INGEST_INTERVAL` (default `60s`) and become searchable.
- **Search** in the WebUI, or `./brainiac search "kafka durability"`.

That's the whole minimum. No cloud, no GPU, no keys.

👉 Want the 10‑minute "record a decision and recall the *why*" walkthrough? → [first-run.md](first-run.md).

---

## Level 1 — Make it your agent's memory (MCP)

Connect Claude Desktop / Cursor / Cline (or your own agent) so it **recalls before it
answers** and **saves what you learn** automatically.

```bash
make mcp-config          # prints the MCP server block to paste into your agent
```

Your agent gets tools: `recall`, `search`, `remember`, `link`, `get_node`,
`add_document`, `ingest`. Nothing else to configure.

---

## Level 2 — Pull in your sources (connectors)

All optional, all off until you set the token. Set these in `.env`, then trigger an
import from the WebUI, your agent's `ingest` tool, or `./brainiac import --source <name>`.

| Source | Turn on with | Notes |
|---|---|---|
| Local files | *(on by default)* | `./data/docs`; formats above |
| GitHub | `GITHUB_TOKEN`, `GITHUB_REPOS=owner/repo` | + `GITHUB_FILES=README*,docs/**` for repo files, `GITHUB_DISCUSSIONS=true` for Discussions |
| Notion | `NOTION_TOKEN` | |
| Slack | `SLACK_TOKEN` | needs `channels:read`+`channels:history`; bot must be in the channel |
| Linear | `LINEAR_TOKEN` | |
| GitLab | `GITLAB_TOKEN`, `GITLAB_PROJECTS` | `GITLAB_BASE_URL` for self‑managed |
| Jira | `JIRA_BASE_URL`, `JIRA_EMAIL`, `JIRA_TOKEN` | |
| Confluence | `CONFLUENCE_BASE_URL`, `CONFLUENCE_EMAIL`, `CONFLUENCE_TOKEN` | |
| Google Drive | `GDRIVE_TOKEN` | needs an OAuth access token |

**Trust:** local files are **trusted** by default (you put them there); remote
connectors are **untrusted** by default (indirect‑injection safety) — recalled
untrusted text is flagged, and any auto‑extracted facts go to a review queue. Vouch
for a source with `<TYPE>_TRUST=trusted` (e.g. `GITHUB_TRUST=trusted`).

---

## Level 3 — Auto‑build the knowledge graph (optional extractor)

By default the graph is filled **by you/your agent** (`remember`/`link`) — highest
quality, no extra model. To also derive nodes/edges from ingested text automatically:

```bash
# In .env — pick ONE:
EXTRACTOR=claude          # needs ANTHROPIC_API_KEY (best quality)
# EXTRACTOR=local-llm     # runs a chat model in Ollama, e.g. EXTRACTION_MODEL=llama3.1
```

Extracted facts land in a **review queue** (WebUI *Proposals* tab / `./brainiac
consolidate`) before going live. Untrusted‑source facts are *always* queued.

---

## Level 4 — Tune ingestion (optional)

| Want | Set |
|---|---|
| Bigger/smaller chunks per source | `sources[].chunk_preset: prose` or `code` (config.yaml) |
| OCR scanned/image PDFs | `OCR_ENABLED=true`, `OCR_COMMAND=tesseract` (install the tool) |
| Remove memory when a file is deleted | `INGEST_PRUNE_DELETED=true` (default: keep) |
| Skip unchanged files faster | auto (mtime) on auto‑import; nothing to set |
| Tune what counts as "relevant" | `RETRIEVAL_MAX_CHUNK_DISTANCE`, `…_GAP`, `…_NODE_…` |

---

## Level 5 — Run it for real (ops)

| Task | How |
|---|---|
| **Back up** (do this before upgrades) | `./scripts/backup.sh` → `backups/*.sql.gz`; restore with `./scripts/restore.sh <file>`. Schedule daily via cron. |
| Reclaim space after deletes | `./brainiac compact` |
| Forget old history | `RETENTION_MAX_AGE=8760h` + `./brainiac sweep-retention` (cron) |
| Keep the hot index in RAM at scale | `TIERING_MAX_HOT_AGE=4320h` + `./brainiac sweep-tiers` (cron) |
| Rebuild vector indexes | `./brainiac reindex` (after `HNSW_M`/`HNSW_EF_CONSTRUCTION` changes) |
| Right‑to‑erasure | `./brainiac erase --node <id>` or `--source <uri>` |
| Use managed Postgres (RDS/Cloud SQL/Neon) | set a full `DATABASE_URL` + [managed-postgres.md](managed-postgres.md) |
| Put it behind HTTPS | `docker compose --profile proxy up`, set `SITE_ADDRESS=memory.example.com` |

More: [operations.md](operations.md) · [deployment.md](deployment.md) · [runbook.md](runbook.md).

---

## Level 6 — Security & multi‑tenant (advanced)

**Enable WebUI writes** (merge/retire/approve) — disabled by default:
```bash
WEBUI_MODE=interactive
AUTH_TOKEN=<long-random>     # paste the same token in the WebUI
```

**Encrypt sensitive text at rest** (defense‑in‑depth on top of disk encryption):
```bash
ENCRYPTION_KEY=$(openssl rand -base64 32)   # back this up separately — losing it = data loss
```
Rotate later: put the old key in `ENCRYPTION_KEYS_RETIRED`, set a new `ENCRYPTION_KEY`,
restart, then `./brainiac reencrypt` and drop the old key. (Caveat: encrypted chunks
aren't matched by keyword search; vector search is unaffected.)

**Multi‑team hard isolation** (one server, many walled namespaces): define principals
in `config.yaml`, give each a `PRINCIPAL_TOKEN_<NAME>`; the MCP process binds to one
via `BRAINIAC_PRINCIPAL_TOKEN`. See [security.md](security.md).

---

## Cheat sheet

**Common commands** — `./brainiac <cmd>`:
`health` · `search "…"` · `recall "…"` · `remember` · `link` · `node` · `import
--source <name>` · `consolidate` · `compact` · `sweep-retention` · `sweep-tiers` ·
`reindex` · `reencrypt` · `erase` · `export`

**Where things live:** `.env` (config) · `./data/docs` (drop‑in files) · `backups/`
(dumps) · `config.yaml` (advanced: sources, principals).

**Deeper docs:** [concepts-and-workflows.md](concepts-and-workflows.md) ·
[api.md](api.md) · [security.md](security.md) · [operations.md](operations.md) ·
[SYSTEM.md](../SYSTEM.md) (the *why* behind every decision).
