# Brainiac — System Specification

> **This is the living spec.** Read it before working on any task. Update it in the *same* PR whenever
> you add, change, or remove a feature, or discover a constraint/edge case. Every "why" that matters
> lives here — code says *what*, SYSTEM.md says *why it is this way*.

**Status:** M0–M5 complete (roadmap + production-readiness hardening; see
[docs/production-readiness.md](docs/production-readiness.md), now a resolved history). Next-generation
work — retrieval quality, scale indexing, connector breadth, security identity/audit, observability — is
tracked in the product-evaluation epics (#202–#209, roadmap #283).

**M0–M4 complete — the full roadmap is done.** capture→recall core (MCP + CLI), ingestion +
density selection, Notion **and** Markdown connectors (plugin seams frozen), read-only + interactive
WebUI (search / recall / consolidation queue / graph / health / system), the librarian pass (CLI + WebUI + cron),
reverse proxy + auth (Caddy), daily backups, recall@k eval, and storage optimizations (reembed, tiering).
Beyond the backlog, work is now maintenance + evolution.
**Source of truth for requirements:** the Memory Platform PRD (v2). This file records how *we* realize it.

---

## 1. What Brainiac is

A **self-hosted, general-purpose memory platform**. It stores not just *what* exists but *why it is this
way* — decisions, trade-offs, rejected alternatives, who and when. Two layers in one Postgres:

- **Layer 1 — semantic search (vectors):** pgvector over curated text chunks. Grows with the corpus.
- **Layer 2 — curated graph (nodes/edges):** every edge carries a `why`, provenance, and author. Grows
  with human effort. Small, never the bottleneck.

Knowledge is captured **through conversation with Claude** ("save what we found: A writes to B; we
rejected sync because of peak load") — Claude parses the structure and calls the core. There is **no
expensive extraction LLM in the default pipeline**. Embeddings run **locally on Ollama**. No cloud-LLM
bill.

Reference (v1) deployment: an engineering team knowledge base over Notion + repos. But the core is
domain-neutral — new domains come from swapping **plugins** and **config**, not forking the code.

---

## 2. Architecture — Core + Plugins + Clients

Three parts with sharp boundaries. This separation is the single most important design rule.

```
 CLIENTS   Claude/MCP        WebUI            CLI          (thin adapters, NO business logic)
                └──────────── all call ────────────┘
                              │  CORE API  (single home of all logic)
 CORE      operations: search · remember · link · recall · supersede ·
 (stable)  consolidate · ingest · flag_stale · confirm · propose_merges · health
           data model (chunks/nodes/edges) · storage · graph algorithms
                │ uses plugins                         │ persists
 PLUGINS   connectors · extractors · selectors ·   Postgres + pgvector (one DB, two layers)
 (swappable) embedders                                 │ embeddings
                                                    Ollama (local)
```

- **Core (stable — build it well, once).** Data model, storage, the operation set, retrieval, the
  consolidation engine. Knows about *chunks, nodes, edges, provenance* — never about "Notion" or any
  domain.
- **Plugins (swappable — grow over time).** Four seams: connectors, extractors, selectors, embedders.
- **Clients (thin adapters).** MCP (Claude), HTTP/REST (WebUI), CLI. They translate user intent into
  core operations and render results — **no logic of their own**.

**Anti-pattern we forbid:** putting `search`/`remember`/`recall` logic inside the MCP server. The moment
the WebUI needs the same behavior it would be reimplemented and the two would drift. All logic lives in
the core; MCP and WebUI both call it. The MCP server is ~50 lines of tool definitions forwarding to core.

**The one rule that protects the project (premature generalization kills projects):**
1. Build the core monolithically and well for the reference domain (Notion + engineering).
2. Draw the four plugin boundaries as interfaces from the start, but implement **one variant each**.
3. Add a second connector (Markdown/Git) only when actually needed — it reveals where the abstraction
   leaks *before* we declare it stable. Generalize against a real second use case, never a hunch.

---

## 3. Technology decisions (and why)

> Each decision records the alternative(s) rejected and the reason, so future changes are informed.

| Area | Choice | Why (and what we rejected) |
|---|---|---|
| **Language** | **Go 1.25+** | The app is an HTTP server + Postgres + Ollama-over-HTTP + MCP tools — **no in-process ML**, so Python's data/embedding-ecosystem edge does not apply here. Go wins on the two hard requirements: a single static binary in a tiny distroless image (trivial deploy) and ~20–50 MB RAM on the shared 4 GB prototype box (OS + Postgres + Ollama). Also matches the goroutly stack. Rejected: Python (heavier RAM/image, separate stack; its only edge — the mature MCP SDK + reference memory server — is easily ported); TS (viable, heavier than Go). |
| **Core shape** | Module `github.com/programmism/brainiac`; package `core` is the sole home of logic | PRD §3.1. Clients (`cmd/mcp`, `cmd/http`, `cmd/cli`) forward to `core`; they never hold logic. |
| **Database** | **Postgres 16 + pgvector** (`pgvector/pgvector:pg16`) | One DB, two layers. Hot path `recall` joins graph→chunks by `source_uri` in one SQL join; one transaction; one backup; consolidation walks both layers as queries. Rejected: graph-in-JSON + separate vector store (cross-store glue, sync risk). |
| **DB access** | **pgx** + **pgvector-go**, raw SQL in a thin repository layer (`internal/store`, functions take a `DBTX` so they run in or out of a tx) | Repositories are the only place SQL lives. Embeddings are sent as **text cast with `::halfvec`** (via `pgvector.HalfVector.String()`) rather than registering the type on connect — registration would fail on a fresh DB before the `vector` extension exists (boot chicken-and-egg). Rejected: heavy ORM (hides vector SQL, fights pgvector operators). |
| **Migrations** | Forward-only SQL files run by a **tiny embedded runner** (`internal/store`, `embed.FS`, applied on boot + `kb migrate`) | ~60 LOC, zero external migration dep, tracked in `schema_migrations`, each file atomic. Schema is stable as we scale (add indexes, quantize — we don't reshape), so a full framework (goose) is unwarranted. |
| **Vectors** | `halfvec(768)` + HNSW on hot tier | nomic-embed-text = 768 dims; halfvec halves RAM at negligible loss. Room to go int8/binary later (§7). |
| **Embeddings** | **Ollama `nomic-embed-text`** (~270 MB, 768-dim) | The genuinely-free workhorse; light on CPU. Embedder is a plugin, so not bound to Ollama. |
| **HTTP API** | **net/http** (stdlib routing, Go 1.22+) + **chi** middleware | Serves WebUI + generic REST. Thin adapter over core; minimal deps. |
| **MCP server** | Official **Go** MCP SDK (fallback `mark3labs/mcp-go`) | Tool shapes referenced from `modelcontextprotocol/servers` *memory* server; we replace its flat-jsonl store with core/Postgres and add the vector ops it lacks. |
| **CLI** | **cobra** | `kb import/refresh/consolidate/reembed/health/migrate` for operators + cron. |
| **Config** | Single **YAML** (`config.yaml`) via `yaml.v3` + env for secrets | All domain specificity in one file (PRD §19). Same engine, different domain = different YAML. |
| **WebUI v1** | One static HTML+JS file embedded via `embed.FS` and served by the Go app (read-only) | PRD §6.2 phasing — removes 80% of "I don't want the terminal" in a few evenings. Interactive (React/Svelte) + graph (Cytoscape/react-force-graph) come later. |
| **Reverse proxy** | **Caddy** (auto-TLS + auth) | Fronts MCP + WebUI; Postgres never exposed (PRD §16). |
| **Deploy** | **Docker Compose**, one command; app image = distroless/static with the Go binary | See §4 — the headline requirement. |
| **CI** | GitHub Actions: `gofmt` + `golangci-lint` + `go test` with a pgvector service container | Gate every PR. |

If any decision here changes, edit this table and add a dated line to §10 (Decision Log).

---

## 4. Deployment — "very easy to deploy" is a hard requirement

The product must be trivial to stand up. The target is:

```bash
git clone https://github.com/programmism/brainiac
cd brainiac
cp .env.example .env      # sane defaults; only secrets to set
docker compose up         # → healthy stack: db + ollama + app
```

Compose (`docker-compose.yml`) brings up:
- **`db`** — `pgvector/pgvector:pg16`, named volume, `pg_isready` healthcheck.
- **`ollama`** — local embeddings; healthchecked via `ollama list`.
- **`ollama-pull`** — one-shot: pulls the embedding model once Ollama is healthy, then exits (idempotent).
- **`app`** — the Go service, a single static binary in a **distroless** image (built from `Dockerfile`).
  On boot it loads config, connects to Postgres, **applies migrations idempotently**, then serves. It
  hard-depends only on `db` (`condition: service_healthy`); Ollama is optional (graceful degradation).

**Self-verification (the app proves its own state):**
- `GET /healthz` — liveness (200 while serving).
- `GET /readyz` — readiness: gates on the DB (`503` if unreachable); reports the embedder as
  `ok`/`unreachable`/`not-configured` but never fails on it (§11). Returns JSON `{"db","embedder"}`.
- Container `HEALTHCHECK` runs `/brainiac healthcheck` (probes `/healthz`; no shell needed in distroless).
- **CI `smoke` job** boots `db + app` via compose, waits for `/readyz` to report `db: ok`, and thereby
  verifies end-to-end that the image builds, the app starts, and migrations apply. This is how we
  validate deploy without local Docker.

**Updating.** `app` is built from the checkout, so an update is get-new-code + rebuild. `./brainiac update`
(#160) does it safely in one command: refuse on a dirty tree → fetch tags → checkout the **latest release
tag** (never `main`) → `docker compose up -d --build` → wait for the **app container's Docker health
status** to reach `healthy` (the container's own `/brainiac healthcheck`, falling back to curl `/readyz`
only if `app` has no healthcheck) → **roll back to the prior ref on failure**. Gating on container health
(not a fixed curl window) means a slow first boot sits in `starting` rather than tripping a false
rollback. Already-latest is a no-op. Scheduling is left to host cron/systemd (a documented recipe), not an
in-app daemon — the update path is a shell operation, kept out of the binary.

Design constraints for deploy:
- **No manual steps** beyond editing `.env`. Model pull + schema migration are automatic and idempotent.
- **Healthchecks + `depends_on: condition: service_healthy`** so the app never races an unready DB.
- **Prototype tier runs on 4 GB** (PRD §12): keep Ollama `num_ctx` small, use `keep_alive` so the
  embedder and any pipeline LLM are not co-resident.
- Production adds **Caddy** (TLS + auth) in front and a **daily `pg_dump`** — see the M4 backlog.

---

## 5. Data model (domain-neutral)

Three tables; vectors and graph in the same Postgres. Full DDL in `migrations/` (mirrors PRD Appendix A).

- **`chunks` (Layer 1)** — `id, text (raw, always stored), embedding halfvec(768), source_uri,
  source_locator jsonb, quality_score, tier(hot|cold), content_hash, discriminators jsonb, scope_key text,
  created_at, source_modified_at`. HNSW cosine index on `embedding WHERE tier='hot'`. `scope_key` (empty =
  global) carries the same identity scope as nodes so the retrieval lens can restrict search to a project +
  global (#119).
  - *Raw text is mandatory:* needed to answer, and to **re-embed on model change without re-reading
    sources** (§7 optimization).
- **`nodes` (Layer 2)** — `id, canonical_name, aliases[], type, summary text, summary_embedding halfvec(768),
  rollup text, status(current|historical), discriminators jsonb, scope_key text, created_at, last_confirmed_at`.
  `rollup` is a curated "current state of X" synthesis over a hub node's edge history (#198) — descriptive
  prose, not identity, so it never affects dedup.
  `summary` is the human-readable description; `summary_embedding` is derived from it and powers semantic
  dedup. Unlike the vector, the text is returned to clients so a recalled/looked-up entity can describe and
  cite itself (#181, Tier 3); pre-Tier-3 nodes stay NULL until re-remembered. **Node identity = `canonical_name` + `discriminators`**
  (the identity-bearing axes: `project`, `env`, …; empty = global/shared). `scope_key` is their canonical
  serialization (sorted `k=v;` pairs, written by the app) and keys idempotent upsert + dedup, so same-named
  entities in different projects stay distinct while universal ones accrue globally (#117, §12).
- **`edges` (Layer 2)** — `id, from_id, to_id, type, why, source_uri, source_locator, author, status,
  superseded_at, created_at, last_confirmed_at`. FK indexes on `from_id`/`to_id`. `superseded_at` (and the
  matching column on `nodes`) is the **valid-time** stamp — set when the row flips to historical — that lets
  memory be queried *as of* a past date (#200); NULL for current rows and for rows retired before valid-time
  existed.

**Design rule:** every edge carries **`why` + provenance + author**. That triple is what makes this a
memory of *decisions*, not a fact dump.

**Supersession, not deletion:** changed decisions add a `supersedes` edge and mark the old node/edge
`status='historical'`. "Why we changed our minds" is the most valued content for onboarding.

---

## 6. Core operation set (the shared API)

Every client calls these; no client reimplements them. Surfaced as MCP tools, REST endpoints, and CLI
commands — same functions underneath.

| Operation | Purpose |
|---|---|
| `search(query, k, filters)` | Vector search Layer 1 (hot tier) → chunks + provenance |
| `remember(node)` | Upsert node with semantic dedup check (flags dups, never auto-merges) |
| `link(from, type, to, why, source, author)` | Insert edge with rationale + provenance |
| `recall(query)` | Vector search + graph traversal (incl. `supersedes` history) + join raw chunks → cited evidence bundle |
| `supersede(old, new)` | Replacement-not-deletion |
| `flag_stale(edge)` / `confirm(edge)` | Staleness lifecycle |
| `propose_merges()` | Dedup candidates for consolidation |
| `consolidate(options)` | The librarian pass (§8) |
| `ingest(source, opts)` | connector → select → chunk → embed → store |
| `health()` | Metrics (§9) |

**Retrieval flow (`recall`):** vector search → entity nodes (lexical name/alias match + precision-gated
`summary_embedding` neighbors, §10) → traverse edges in relevance order → join raw chunks by `source_uri`
→ Claude synthesizes **with citations**. Every claim maps to `source_uri` + locator. **An answer without
a source is a quality bug.**

**Capture flow (default, chat-driven):** human investigates → tells Claude to save it → Claude calls
`remember`/`link` → core upserts node(s) + edge (with `why`, provenance, author) in one transaction. No
pipeline LLM; the "extraction" is the chat itself.

---

## 7. Plugin seams + ingestion + storage optimizations

**Four seams** (interfaces from the start, one impl each for v1):
- `SourceConnector.fetch()/watch()` — "give me documents, tell me when they change." v1: Notion.
- `Extractor.extract(chunk)` — text → nodes/edges. Default: **chat-driven** (bypassed; Claude supplies
  the structure). Two server-side extractors exist for automated bulk paths: **`local-llm`** (Ollama chat
  model + structured output, `extraction.default: local-llm`) for a self-hosted box, and **`claude`**
  (Claude Messages API structured output, `extraction.default: claude` + `ANTHROPIC_API_KEY`, #235) for
  the strongest quality when an API key is available (`internal/plugins/anthropic`, raw HTTP, no SDK — a
  cheaper model like `claude-haiku-4-5` can be set via `extraction.model`). Extracted nodes/edges default
  to the **review queue** (`extraction.review`, see §8). Runs on kept (hot) chunks during ingest,
  best-effort — a failed chunk (or a Claude `refusal`) is counted and skipped, never failing the document.
  Per-chunk today; the Batch API cost path and cross-doc entity resolution are follow-ups (#326).
- `Selector.score(chunk)` — the water filter; keep/queue/drop. v1: `density-filter`.
- `Embedder.embed()/dims()` — v1: Ollama nomic-embed-text.

**Ingestion pipeline (selection *before* the index — PRD §8):** structural filter (free rules) → density
heuristic (unique nouns/terms, entities/numbers) → chunk then select **per-chunk** → LLM gatekeeper on
the borderline queue only (Ollama small model *or* deferred Claude batch) → embed + store raw text +
provenance + `quality_score`. Thresholds are **reversible** because raw text + score are stored.
Ingest decides skip/drop/keep in one pass, then **embeds the survivors in a batch** (`plugins.BatchEmbedder`,
`embedding.batch_size`, default 32) so a large import costs dozens of round-trips, not one per chunk (#140).
Source text is **normalized once before chunking** (CRLF→LF, trailing spaces stripped, blank-line runs
collapsed, trimmed — `core.normalizeText`, #146): a pure idempotent function, so boundaries stay
content-defined; it fixes formatting only and never drops content words. **Chunks overlap** (#214): every
chunk after the first is prefixed with a bounded (≤ `overlapMax` = 256 B), sentence-aligned tail of the
previous chunk, so a fact straddling a boundary lands whole in at least one chunk. The overlap is a function
of local content, so the self-healing property holds — an edit's blast radius just grows by one chunk (the
one whose overlap changed); near-duplicate results the overlap creates are collapsed at retrieval (#217).
`IngestOptions.DryRun` (CLI `kb import --dry-run`) runs chunk + select only — no embed, no write — and
reports what *would* happen (chunk count, kept/queued/dropped/skipped, would-delete), to preview a large or
wrongly-scoped import before committing (#142). `IngestOptions.OnProgress` emits a running
`IngestProgress{Doc, Embedded, ToEmbed, Stats}` between embed batches so a long import isn't a black box
(#139): the CLI renders `embedding <doc>: X/Y chunks` to stderr, auto-import logs large-doc completion. It's
opt-in — without a callback the single-shot batch path is unchanged.

> **Day-one:** the ingest script is *not* required. Prototype ingest goes **through Claude** (paste a
> link/export; Claude reads → selects → calls `remember`/`link`). Write connector automation only when
> ingest becomes a repeatable bulk/scheduled routine.

**Storage optimizations (apply progressively, by need — PRD §13):** selection (strongest lever) →
quantization (`halfvec` → int8 → binary + re-rank) → Matryoshka dim reduction (768→256) → hot/cold
tiering → **re-embed from stored raw text** on model upgrade (why raw text is mandatory).

**Retention on source deletion (intentional).** Editing a source updates its chunks (per-source
reconcile); **deleting a source file does NOT remove already-imported content** — Brainiac is a *memory*,
so what it learned persists even after the source disappears. Ingest only reconciles sources it still
sees; it never prunes vanished ones (#107, decided to keep). To drop content: `docker compose down -v`
(wipe all) or delete the specific rows in Postgres.

---

## 8. Consolidation ("Librarian" pass)

Scheduled or on-demand; walks the graph (small), not the corpus. Drivable by Claude-in-chat or a local
Ollama LLM; reviewable in the WebUI consolidation queue.

**Extraction review queue (the `local-llm` extractor's gate, §7).** When the optional local extractor is
on, its output is written with a third node/edge status — **`proposed`** — alongside `current` and
`historical`. Every read (search, recall, graph, dedup) already filters `status = 'current'`, so proposed
rows are **invisible to the memory until approved**: a weaker local model can suggest structure without
polluting recall. Review is exposed on all three surfaces — WebUI **Proposals** tab, MCP `proposals` /
`review_proposal` tools, and `GET /api/proposals` + `POST /api/proposals/{nodes|edges}/{id}/{approve|reject}`.
**Approve** flips a row to `current` (approving an edge also promotes its endpoints, so a live edge never
dangles off an invisible node; if a current edge already covers that `(from,to,type)` the proposal is
retired instead); **reject** flips it to `historical` (kept as a record, never deleted). Provenance is
stamped `author=local-llm`. The gate is a config toggle — `extraction.review` (default `true`);
`review: false` writes extracted rows straight to `current` for an operator who trusts their model.

1. **Node dedup / canonicalization** — propose merges by name similarity or `summary_embedding`
   proximity, **within a single identity scope** (proposals never cross `scope_key`, so same-named entities
   in different projects are never merged, #118); **human confirms** (auto-merge collapses real entities —
   always reversible, alias history kept). Without this the graph fragments into disconnected islands.
1a. **Split candidates (the mirror of dedup)** — propose **splits** for a node whose current edges contradict
   (same `from`+`type`, different targets): a signal it conflates two entities. The report lists the node with
   its edges; a human routes them with `Split(nodeID, axis, routes)` (CLI `kb split`, `POST /api/split`),
   which carves the node into scoped children (`{axis:value}`) and retires the emptied parent. Reversible.
   Reactive counterpart to `disambiguate` (which moves a whole node); together they cover both conflation
   shapes (#126/#127).
2. **Replacement, not deletion** — `supersedes` edge + `status=historical`.
3. **Staleness** — Consolidate auto-flags an edge "possibly stale, verify" when its source changed since we
   recorded it: a chunk for the edge's `source_uri` has `source_modified_at > COALESCE(last_confirmed_at,
   created_at)` (`store.FlagStaleBySource`, #147). Comparing against `last_confirmed_at` means a **confirmed**
   edge isn't re-flagged until the source changes *again* (no loop). Flags for review only — `Confirm` clears
   it; nothing is superseded automatically. Manual `flag_stale` still works alongside.
4. **Conflict detection** — surface contradictions (same `from`+`type`, different targets) for human
   resolution, carrying both edge ids. Node/edge `type` is **normalized on write** (`core.normalizeType`:
   case + separators folded, `writes-to`/`writesTo` → `writes_to`; synonyms untouched — #156) so a
   contradiction written with a different spelling still matches. Resolve by **retiring the losing edge**
   (`RetireEdge` → `status =
   historical`, the edge-level mirror of supersession): WebUI "keep « X »" buttons, `POST
   /api/edges/{id}/retire`, or CLI `kb retire-edge <id>`. Replacement, not deletion — recall still reaches
   the retired edge via history. Detection is automatic; resolution stays an explicit human action (#148).
5. **Rollups** — a node with many edges gets a "current state of X" summary linking to detailed history;
   creates the two reading levels (*what is now* over *how we got here*).

The librarian pass is **mandatory, not optional** — skipping node dedup is the top failure mode (§11).

**Running it.** `core.Consolidate()` proposes; it never auto-applies. Surfaces:
- **CLI:** `kb consolidate` prints the report; `kb merge --keep <id> --drop <id>` applies an approved merge
  (`Core.ApplyMerge`), `supersede`/`link` handle the rest.
- **Schedule:** external cron, e.g. weekly `0 3 * * 0 kb consolidate` (matches `consolidation.schedule` in
  config). A scheduled run surfaces candidates for review; humans approve merges via the CLI or the WebUI
  consolidation queue (#25).

---

## 9. Health, scaling, evaluation

**Metrics (from day one; ★ = load-bearing for scaling):** ★ vector index size vs RAM (healthy: index
< ½ RAM), ★ query p95 latency (< ~200 ms, rises on disk spill), recall@k on the golden set, chunk count
(hot/cold), node/edge count + edges/node, % edges stale-flagged, % nodes historical, ingest throughput,
open conflicts, capture rate (saves/week — adoption; friction kills the system).

**Scaling is observed, not theoretical.** The binding constraint is the vector index in RAM
(~3 KB/vector float32; halved with halfvec). Act when the index approaches ~½ RAM, **or** p95 climbs,
**or** golden-set recall degrades — not at an abstract row count. Rough tiers: 100–300K chunks = 4 GB
prototype OK; ~1M = first wall (move off 4 GB); ~10M = 32–64 GB node; ~100M = quantize + shard or a
dedicated vector DB.

**Operational (system) metrics — the WebUI "System" tab + `GET /api/system` (#132).** A point-in-time
snapshot so an operator sees when the deployment is approaching its allocated-resource ceiling — distinct
from the corpus "Health" tab above. Three sections, roll up to a `status` (`ok`/`warn`/`critical`) with
human-readable warnings; thresholds live in `core` (`SystemMetrics`) so every client agrees:
- **Container memory** — the cgroup limit vs current usage (`internal/sysstat`, best-effort: reads
  cgroup v2 then v1; reports `available:false` off-Linux, e.g. local dev, rather than a wrong number).
  This is the "am I hitting my `mem_limit`?" signal (the app runs under `mem_limit: 256m` in compose).
  Warn ≥ 85%, critical ≥ 95%.
- **Database** — `pg_database_size`, the ★ hot vector-index bytes, active connections vs `max_connections`
  (warn ≥ 80%), and pgx pool saturation (acquired vs size, warn ≥ 80%).
- **Process** — the Go runtime's own footprint: heap in-use/reserved, goroutines, GC cycles, CPU cores,
  uptime.
These are cheap catalog/runtime reads, not history; long-run time-series is a separate concern (Prometheus
scrapes `/metrics`). A DB read failure is non-fatal to the snapshot but downgrades `status` to `critical`.

**Logs — structured JSON to stdout, with the WebUI "Logs" tab + `GET /api/logs` as a convenience (#166,
#258).** Both log streams are **structured JSON on stdout** (Docker's json-file driver rotates it, so the
durable log survives a crash): the chi **access log** (one object per request, with `request_id`) and the
**application logger** — startup, migrations, auto-import, reload, and the ≥500 lines the server records.
The app logger is `internal/applog` (an slog JSON/text handler) which also **bridges the standard library
`log`**, so the many existing `log.Printf` call sites emit the same structured records without a rewrite.
Format and level are config (`logging.format` json|text, `logging.level` debug|info|warn|error; env
`LOG_FORMAT`/`LOG_LEVEL`). Both streams are still teed into a bounded in-memory ring (`internal/logbuf`, last
~2000 lines, no disk). The tab shows them newest-last with per-line and "Copy all" copy buttons plus
optional auto-refresh, so an operator can grab an error (e.g. a failed conflict resolution) without shell
access to the container. Lines are **redacted of obvious secrets** (PATs, `Bearer …`, `token=…`) at
capture time, so neither the API nor the UI echoes a credential. Same open-read posture as `/system`
(protect the whole surface via the reverse proxy); the endpoint is mounted only when the log sink is wired
(it is, in `cmd/http`). The MCP server is a separate stdio process and logs to its own stderr — outside
this viewer — but it now logs **structured JSON to stderr** (stdout is the stdio protocol channel) via the
same `internal/applog`.

**Evaluation:** a golden query set (~20–50 questions with expected sources) run at every notable growth
step and after model/threshold changes; citation discipline (uncited answer = quality bug); capture rate
as the adoption signal.

---

## 10. Decision Log

Newest first.

- **2026-07-18** — **Structured JSON app logger + stdlib bridge (#258, observability P1).** The access-log
  half of #258 already emitted JSON to stdout with a `request_id` (2026-07-17); the **application** logger was
  still stdlib `log` in plain text on **stderr**, so the durable machine-parseable log covered requests but
  not startup / migrations / auto-import / the ≥500 lines the server records. Added `internal/applog`: an
  slog **JSON** (or `text`) handler writing to stdout, teed into the 2000-line ring, at a config level. It
  **bridges the standard library `log`** (`log.SetOutput` → an slog info record per line, flags cleared), so
  the many existing `log.Printf` call sites across `cmd/` and `internal/server` emit structured records with
  no rewrite. Format/level are config (`logging.format`, `logging.level`; env `LOG_FORMAT`/`LOG_LEVEL`,
  validated). The MCP process adopts it too but writes to **stderr** — stdout is its stdio protocol channel,
  so app logs there must never touch it. Unit-tested (JSON/text render, level filter, ring tee, stdlib
  bridge) + a config test for the new fields + validation. Deferred to a follow-up: threading a
  request-scoped logger through core so *app* lines inside a request also carry the `request_id` (today only
  the access log does).
- **2026-07-18** — **One-shot `capture` macro + verb tiering (#281, DX P1).** The write surface exposed
  `remember` + `link` as co-equal primitives, so recording an everyday decision meant two calls (create
  entity, create entity, link) and a mental model of nodes-vs-edges before you could save anything. Added a
  **`capture`** MCP verb that wraps the core (optional `Remember` for each endpoint's one-line summary, then
  `Link` for the relationship + `why`) into a single call — both entities created and linked at once, the
  simplest path to memory. Docs (`concepts-and-workflows.md`) now split the verbs into **everyday**
  (`capture`/`search`/`recall`, with `remember`/`link` as the finer writes underneath) and **advanced
  curation** (`disambiguate`/`supersede`/`rollup`/`as_of`) that the consolidation pass and operators own —
  agents shouldn't reach for them mid-conversation. Pure logic stays in the core (`capture` composes existing
  `Remember`/`Link`, no new business logic in the adapter); covered by the DB-gated MCP round-trip test
  (capture → recall sees the edge; the optional summary makes the entity searchable).
- **2026-07-18** — **Passage-level provenance (#243, ingestion P1).** A chunk's `source_locator` carried only
  the document-level pointer (path / page id), so a citation could name the doc but not *where in it*. The
  chunker now exposes `SplitWithProvenance` — same overlapped chunk text as `Split` (so content hashes /
  reconcile are unchanged), plus each chunk's **byte offset** (its content-defined core's untrimmed start in
  the normalized text) and the **nearest preceding Markdown heading** (`precedingHeading`/`atxHeading`).
  `ingestDoc` stamps these per chunk into `source_locator` (`char_offset` + `heading`, merged over the doc
  locator, copied so chunks don't alias the shared map), so search/recall hits point at a passage/section.
  Pure chunk unit tests (offsets increase, text matches `Split` 1:1, heading detection incl. indented /
  closing-`#` / too-many-`#`) + a DB-gated ingest test (stored chunks carry `char_offset` and the section
  heading). Block/offset anchors for non-Markdown formats can extend `SplitWithProvenance` later.
- **2026-07-18** — **Subsystem telemetry counters (#319, observability P1).** #259 shipped per-route HTTP
  metrics but left the *subsystem* signals from its list unbuilt. Added them: the `metrics` registry gained
  **counter** support (`SetCounter`, rendered as Prometheus `TYPE counter` so `rate()`/`increase()` work),
  and `brainiac_ingested_chunks_total` + `brainiac_extract_failures_total` are exposed from process-lifetime
  atomics on `Core` (incremented in `ingestDoc` per stored chunk / on each extraction failure). The
  **consolidation queue depth** is a new gauge `brainiac_review_queue_depth` = proposed nodes+edges, added to
  `HealthCounts` (one extra subquery). The embedding backlog was already `brainiac_chunks_cold`. A matching
  `BrainiacHighExtractionFailureRate` alert (failures/ingested > 10%) + runbook entry tie this to #264.
  Unit test for counter rendering (no DB) + DB-gated test (ingest bumps the counter, a proposed row shows in
  the review queue). Closes the #259 follow-up.
- **2026-07-18** — **Linear connector (#240, ingestion P1).** A sixth connector over the stable
  `SourceConnector` seam — and the cleanest of the Confluence/Jira/**Linear** bundle, since Linear issue
  descriptions are already **Markdown** (no format conversion). `internal/plugins/linear` runs a paginated
  GraphQL query (`issues(first, after){ nodes{ identifier title description updatedAt url } pageInfo }`)
  against the Linear API with the key in the `Authorization` header (Linear's raw-key convention, not
  Bearer), surfacing GraphQL `errors` as a Go error. Each issue → one `RawDoc` (`url` provenance, `updatedAt`
  → `ModifiedAt`); empty ones skipped. Wired like the others: `LINEAR_TOKEN` auto-creates a `linear` source;
  MCP `import` / CLI `kb import --source linear`. Unit-tested against a fake GraphQL endpoint (cursor
  pagination, empty-skip, graphql-error). **Jira** (ADF descriptions) and **Confluence** (XHTML storage
  format) need format conversion like `doctext` and are tracked as follow-up #343.
- **2026-07-18** — **Google Drive connector (#239, ingestion P1).** A fifth connector over the stable
  `SourceConnector` seam. `internal/plugins/gdrive` lists the files an OAuth **access token** (`GDRIVE_TOKEN`)
  can see (`GET /drive/v3/files?q=trashed=false`, `nextPageToken`-paginated) and pulls their text: **Google
  Docs** are exported to `text/plain` (`/files/{id}/export?mimeType=text/plain`), `text/*` files are
  downloaded (`?alt=media`); folders, Sheets/Slides, and binaries are skipped, and a per-file error is
  non-fatal (#241). Each file → one `RawDoc` (`webViewLink` provenance, `modifiedTime` → `ModifiedAt`). Wired
  like the others: `GDRIVE_TOKEN` auto-creates a `gdrive` source; MCP `import` / CLI `kb import --source
  gdrive` dispatch to it. Fully unit-tested against a fake Drive API (Doc export vs text download, folder/PDF
  skip, 401→error) — no live token. **Minting/refreshing** the OAuth token is deliberately out of scope (the
  per-source credential/OAuth store is #246); Sheets/Slides export and Drive change-token incremental sync
  can follow (#323).
- **2026-07-18** — **GitHub connector (#238, ingestion P1).** A fourth connector over the stable
  `SourceConnector` seam. `internal/plugins/github` reads a repo's **issues + pull requests** (title/body)
  over the GitHub REST API (`GET /repos/{owner}/{repo}/issues?state=all`, which returns PRs too — tagged
  `kind: pr` by the `pull_request` field), page-paginated (stop on a short page), token-auth (`Authorization:
  Bearer`, `X-GitHub-Api-Version`). Each issue/PR → one `RawDoc` (`html_url` provenance, `updated_at` →
  `ModifiedAt`); empty ones skipped, and a per-page fetch error is non-fatal (#241). Wired like the other
  connectors: `GITHUB_TOKEN` auto-creates a `github` source, repos come from `sources[].repos` /
  `GITHUB_REPOS` / the import target, dispatched by MCP `import` and CLI `kb import --source github [--path
  owner/repo]`. A new `SourceConfig.Repos` field carries the repo list. Fully unit-tested against an httptest
  fake GitHub API (pagination, PR vs issue tagging, empty-skip, 401→error) — no live token. **Note:** the
  ambient `GITHUB_TOKEN` in CI/Actions meant the config tests had to clear it to stay deterministic. GitLab,
  code/file ingestion, and Discussions (GraphQL) are tracked as follow-up #340.
- **2026-07-17** — **Postgres tuning-at-scale guide (#232, scale P2).** `pgxpool` was fully defaulted and
  there was no guidance for the churn Brainiac generates. Added a **"Tuning Postgres at scale"** section to
  `operations.md`: (1) **autovacuum** — supersede/merge flip rows `current→historical` and ingest deletes
  stale chunks, so per-table `autovacuum_vacuum_scale_factor = 0.05` on `chunks`/`nodes`/`edges` keeps dead
  tuples in check; (2) **HNSW maintenance** — `REINDEX INDEX CONCURRENTLY` the partial hot-tier/summary
  vector indexes after big re-embeds, watching `brainiac_vector_index_bytes` vs the ★ ½-RAM ratio; (3)
  **PITR** — `wal_level`/`archive_mode`/`archive_command` + `pg_basebackup` for point-in-time recovery beyond
  the daily `pg_dump`. The **pool-sizing** and **PgBouncer** knobs from #232 were documented with #253
  (managed-postgres.md, `pool_max_conns` in the DSN). Docs only.
- **2026-07-17** — **Managed / external Postgres guide + override (#253, deploy P2).** The compose hardcoded
  the app's `DATABASE_URL` at `db:5432?sslmode=disable`, and there was no story for pointing at RDS / Cloud
  SQL / Neon. Added `docker-compose.managed.yml` (same override pattern as the GPU file, #252): it reads
  `DATABASE_URL` from `.env` (required, `:?`-guarded), drops the app→`db` dependency, and parks the bundled
  `db`/`backup` services behind an inactive `profiles:` so they don't start. `docs/managed-postgres.md`
  covers pgvector-on-managed, **TLS DSN modes** (`require` vs `verify-full` + `sslrootcert`), **pool sizing**
  via the DSN's `pool_max_conns` (pgxpool reads it natively — no rebuild) kept under the instance's
  `max_connections`, **PgBouncer** for many MCP-process pools, and skipping the bundled backup cron (the
  provider handles PITR). Linked from `deployment.md` + `.env.example`. Docs/config only; base compose
  unchanged (still what CI smoke-tests). This also documents the pgxpool tuning knob half of #232.
- **2026-07-17** — **Orphan + age-based staleness sweeps in Consolidate (#263, observability P2).** Staleness
  only fired when a *source chunk changed* (`FlagStaleBySource`), so **chat-captured** edges (no `source_uri`)
  and edges whose **source vanished** never aged out; and there was no sweep for **orphan** (edgeless) nodes
  left behind after their relationships were retired/superseded. Added two read-only sweeps to the
  `ConsolidationReport`: **`Aging`** — current, unflagged edges whose `COALESCE(last_confirmed_at, created_at)`
  predates `EdgeStaleAge` (180 days), catching time-decayed decisions regardless of source; and **`Orphans`**
  — current nodes with no current edge on either end. Both are **proposals only** (the report never mutates,
  #262) — surfaced in `/api/consolidate` JSON and the `brainiac consolidate` CLI (with `confirm`/`retire-edge`/
  link/delete hints). New store queries `FindAgingEdges`/`FindOrphanNodes`; DB-gated test (orphan surfaces,
  linked nodes don't; a fresh edge isn't aging, a 365-day-old one is).
- **2026-07-17** — **Prometheus alert rules + operational runbook (#264, observability P2).** The app
  exposed `/metrics` (per-route latency + error counts #259, graph-health gauges, container memory, vector
  index size) but shipped **no alerts and no runbook** — operators had to invent both. Added
  `deploy/monitoring/brainiac.rules.yml` (a Prometheus alert group: `BrainiacDown`, `HighErrorRate` from the
  5xx ratio, `SlowSearchP95` via `histogram_quantile` on the per-route buckets, `VectorIndexExceedsHalfRAM`
  and `MemoryNearLimit` tied to the ★ index-vs-RAM scaling ratio in §9/#256, and `HighStaleEdges` /
  `HighHistoricalNodes` curation signals), a `prometheus-scrape.yml` wiring snippet, and `docs/runbook.md`
  with per-alert first-response steps. Every threshold matches a documented scaling target; `/metrics` is
  flagged unauthenticated (scrape internally, never public). Docs/config only — no app change.
- **2026-07-17** — **GPU compose override + scaling doc (#252, deploy P1).** GPU appeared in the docs only
  as "you don't need one" — the *scale-to-strong-hardware* promise had no compute-axis story. Added
  `docker-compose.gpu.yml`: a thin override that reserves an **NVIDIA GPU** for the Ollama service
  (`deploy.resources.reservations.devices`, `driver: nvidia`) and lifts the 4 GB-box `mem_limit`/`cpus`
  caps, applied with `-f docker-compose.yml -f docker-compose.gpu.yml`. Nothing else changes — app/DB/
  migrations/`./brainiac` are identical; it only moves embedding (and the optional local-LLM extractor) onto
  the GPU. Documented in `deployment.md` (a "Scaling on the compute axis — GPU" section with the toolkit
  prerequisite and an `nvidia-smi` verify step) + a README FAQ entry. Deploy-config + docs only; the base
  compose (unchanged) is still what CI smoke-tests.
- **2026-07-17** — **TLS posture + basic-auth is not the tenant boundary (#271, security P1).** Layer-1
  reads are fully open (protection depends on the proxy), the shipped Caddy used a **single shared**
  `basic_auth` credential, and `SITE_ADDRESS` could be `:80` — so bearer/basic-auth crossed the wire in
  cleartext. Hardened the Caddyfile: added an **HSTS** header (`Strict-Transport-Security`, so a browser that
  has seen HTTPS won't downgrade) and reframed `:80` as **local-dev only** with a prominent warning that the
  shared `basic_auth` is a **coarse network gate, not the multi-tenant boundary** — the per-team boundary is
  **Layer-2 principals** (`principals:`, per-token namespace isolation, #120), which a single shared
  credential can't provide. Docs updated to match (`security.md` threat #2 + hardening checklist,
  `.env.example`). No app-code change — the app can't see the upstream TLS; this is proxy hardening + an
  operator-posture correction. The `caddy` CI job validates the Caddyfile.
- **2026-07-17** — **Calibrated (relative) chunk distance gating (#215, retrieval P1).** Chunk retrieval
  gated on a **single absolute** cosine cutoff (`MaxRelevantDistance = 0.75`) — non-portable across
  model/domain/query-length: a weak query with only mediocre matches still returned a long barely-relevant
  tail. Added **relative** gating in `filterByDistance` — a chunk is also dropped once it sits more than
  `ChunkDistanceGap` (0.15) behind the **best** (nearest) hit — so a strong query keeps its tight cluster
  while a weak one is trimmed to its few genuinely-close hits. Mirrors the node gate already in recall.go
  (`MaxNodeDistance` + `NodeDistanceGap`). Both gates are monotonic over the nearest-first list, so it still
  returns a prefix. Pure unit test in `search_test.go` (absolute-only, relative-gap-dominates, weak-query
  cluster). Eval-tunable (#29); making the thresholds **config** rather than consts (the issue's second
  half) is tracked as follow-up #332.
- **2026-07-17** — **Non-fatal fetch errors in ingest (#241, ingestion P1).** A single fetch/pagination
  error from a connector aborted the **entire** import (`return stats, err`) — so a large Notion/Slack
  backfill was fragile: one bad page lost the whole run. `Core.Ingest` now **counts and skips** a yielded
  fetch error (`IngestStats.FetchErrors`) and keeps consuming whatever the connector yields next; only a
  cancelled context is terminal. Connectors that continue past an error (markdown per-file, Slack
  per-channel) now import all the good items; ones that stop (Notion) return partial success with a count
  instead of a hard failure. Surfaced in the auto-import log. **Resume** of an interrupted backfill rides on
  the per-source **mtime skip** (#236, `Incremental`): a re-run skips already-synced docs. Persisted
  connector-level cursors (Notion `start_cursor`, etc.) remain part of #323. DB-gated test in `ingest_test.go`
  (a mid-stream error connector → both good docs import, `FetchErrors=1`, no error).
- **2026-07-17** — **Structured JSON access logs + request-id (part of #258, observability P1).** The HTTP
  access log was chi's plain-text formatter teed to stderr + the 2000-line in-memory ring — lossy, and not
  machine-parseable. Replaced it with a `jsonLogFormatter` that emits **one JSON object per request** to
  **stdout** (Docker's json-file driver rotates it, so the durable request log survives crashes), teed to
  the ring only as a WebUI convenience. Each line carries the chi **request-id** (`request_id`) for
  cross-line correlation, plus `method`/`path`/`status`/`bytes`/`duration_ms`/`remote`/`ts`; the **path only**
  is logged, never the query (which can carry secrets), verified by a unit test. The app's operational
  `log.Printf` lines (startup, migrations, notices) stay plain on stderr for now — structuring those is a
  tracked follow-up (#329).
- **2026-07-17** — **First-run tutorial + troubleshooting (#276, product-docs P0).** Onboarding stopped at
  "it's running" — no end-to-end example, no troubleshooting, and MCP setup looked like it needed a
  hand-edited absolute path. Added `docs/first-run.md`: a 10-minute CLI-only walkthrough (`up` → `remember`
  two nodes → `link` with a `--why` → `recall` the rationale back out → optional bulk `import` → wire MCP),
  a **troubleshooting table** (model-still-downloading / `/readyz` 503, the `embedding.dims` schema-mismatch
  error, embedder-unreachable, port-busy, MCP-not-connected, writes-off-by-default), and an **MCP verify
  step** — pointing at `./brainiac mcp-config` (which already emits the absolute path via `$(pwd)`, so no
  hand-editing) plus a "ask Claude to recall" check. Linked from README + laptop.md. Screenshots can't be
  rendered in CI, so they're tracked as a capture-on-deploy checklist in `docs/images/README.md` (the
  tutorial reserves the filenames). Docs-only.
- **2026-07-17** — **Claude-backed extractor (#235, ingestion P0).** The default extraction path is
  chat-driven (Claude via MCP supplies structure) and the only *server-side* extractor was `local-llm`
  (Ollama) — weaker, and needing a beefy box. Added `internal/plugins/anthropic`: a `plugins.Extractor`
  over the **Claude Messages API** with a JSON-schema **structured output** (`output_config.format`), so bulk
  ingest gets high-quality automated extraction wherever an `ANTHROPIC_API_KEY` is available. Kept to **raw
  HTTP, no SDK** (minimal-dependency stance, §3) — mirrors the hand-rolled Ollama client, with retry/backoff
  and `stop_reason: "refusal"` surfaced as a per-chunk error (skipped, chunk still stored). Selected via
  `extraction.default: claude`; `extraction.model` defaults to `claude-opus-4-8` but can point at a cheaper
  model (e.g. `claude-haiku-4-5`) for bulk cost; extracted rows still default to the review queue. Both
  `cmd/http` and `cmd/mcp` `extractorOptions` now switch claude → local-llm → chat-driven. Config validates
  that a key is present when `default: claude`. Unit-tested against an httptest fake Messages API (structured
  parse + empty-field drop, refusal→error, non-200→error, model override) — no live key needed. **Batch API**
  cost path and **cross-document entity resolution** (the issue's second half) need an ingest-side batching
  seam and are tracked as follow-up #326.
- **2026-07-17** — **Slack connector (#237, ingestion P0).** A third real connector over the stable
  `SourceConnector` seam (after Notion + local files), validating the seam again against a chat source.
  `internal/plugins/slack` reads the Slack Web API with a bot token (`SLACK_TOKEN`, scopes
  `channels:read`+`channels:history`): `conversations.list` enumerates the readable public channels (or an
  explicit set via `NewForChannels`), `conversations.history` pulls each channel's messages, both paginated
  by `response_metadata.next_cursor`, with 429/`Retry-After` backoff and the `{ok,error}` envelope surfaced
  as an error. Each substantive message becomes one `RawDoc` (`slack://<channel>/<ts>` provenance, message
  `ts` → `ModifiedAt`); system/join/blank messages (non-empty `subtype` or empty text) are skipped. Wired
  everywhere Notion is: `SLACK_TOKEN` auto-creates a `slack` source, and MCP `import` / CLI `kb import
  --source slack [--path CHANNEL]` dispatch to it. Fully unit-tested against an httptest fake Slack server
  (pagination, channel-filter skips `list`, `ok:false` → error, ts parsing) — no live workspace needed.
  Threads (`conversations.replies`) and persisted per-channel cursors are future refinements (#323).
- **2026-07-17** — **Persisted incremental-sync state + mtime skip (#236, ingestion P0).** Auto-import
  re-scanned and re-reconciled every file each interval. Added a `source_sync` table (migration `0015`,
  one row per `source_uri` with the last-synced `modified_at`) and an opt-in `IngestOptions.Incremental`:
  when set, `ingestDoc` skips a document whose `RawDoc.ModifiedAt` has not advanced past the stored value —
  before any chunking/hashing/embedding — and records the sync point inside the reconcile transaction. The
  timer-based **auto-import turns this on**; the one-shot `import` leaves it off so it still does a full
  content-hash reconcile (mtime is only *trusted* when the caller opts in, since a hand-reset mtime could
  otherwise mask a content edit). DryRun never skips (its preview must be exact). DB-gated test in
  `ingest_test.go` (skip on equal mtime, process on newer mtime, full reconcile when non-incremental).
  Consuming `Watch()` for **deletion** sync and connector-side read-skipping (so unchanged files aren't even
  read) need cross-connector scoping and are tracked as follow-up #323.
- **2026-07-17** — **Multi-format extraction layer (#234, ingestion P0).** The local-file connector ingested
  only `.md`/`.markdown`; a folder of HTML exports or `.docx` was invisible. Added `internal/doctext` — a
  **dependency-free** extraction seam (`ToText(name, data)` / `Supported(name)`) that turns a document's
  bytes into plain text: Markdown/plain text pass through, HTML is stripped with a small hand-rolled
  tokenizer (drops `<script>`/`<style>` bodies, block tags → line breaks, decodes common entities), and DOCX
  is unzipped and its `<w:t>` runs pulled from `word/document.xml` with the standard library (namespace-prefix
  agnostic). The `markdown` connector now walks every `Supported` file and converts it, keeping `.md`
  behavior and the `markdown://` source-URI scheme unchanged (so existing docs don't churn) — `/data/docs`
  now ingests `.txt`/`.html`/`.docx` too. Fixture unit tests in `internal/doctext` (HTML strip + entity
  decode + unterminated-tag safety, DOCX runs/paragraph/tab, invalid-zip, passthrough) and the connector
  test. **PDF** needs a heavier parser (external lib / OCR), so `ToText` returns `ErrUnsupported` for it and
  those files are skipped+counted; tracked as follow-up #321. Google Docs extraction belongs to the Drive
  connector (#239).
- **2026-07-17** — **Per-route latency + error-rate metrics (#259, observability P1).** The single latency
  histogram lumped `/healthz`, `/metrics`, and static assets in with `/api/search`, polluting the p95
  scaling signal, and there was no error counter. Added, alongside the unchanged overall histogram (kept for
  existing dashboards): **`brainiac_http_route_duration_seconds{route}`** — a latency histogram per matched
  route — and **`brainiac_http_requests_total{route,code}`** — a per-route, per-status request counter (the
  error-rate signal). Cardinality is bounded by using chi's **matched route pattern** (e.g. `/api/search`,
  `/api/edges/{id}/confirm`), not the raw path; unmatched paths collapse to `other`. The `metrics` package
  stays router-agnostic — a `routeMetrics` middleware in `server` extracts the chi pattern and status (via a
  `statusRecorder` that still forwards `Flush` for SSE) and calls `reg.ObserveRoute`. Unit tests in
  `metrics` + `server` (no DB). The subsystem throughput/queue-depth/extraction-failure counters from the
  issue need cross-layer core hooks and were split into a tracked follow-up (#319); #259's HTTP-metrics core
  is done here.
- **2026-07-17** — **Startup banner (#254, deploy P2).** First boot logged pieces (migrations, "listening
  on …") but never the actionable summary a novice needs, so getting to the WebUI leaned on reading
  `laptop.md`. `brainiac-http` now prints a one-block banner once it's up: the clickable **WebUI URL**
  (`webURL` maps `:8080`/`0.0.0.0`/`::` to `localhost`), the health/ready endpoints, the WebUI write mode,
  a hard-isolation note, and — the common first-run gotcha — whether the **embedder model is still
  downloading / unreachable** (search-and-recall 503 until it's pulled, #250). The old standalone one-shot
  embedder-warning block was folded into the banner so the caveat logs once, next to the URL. Pure `webURL`
  unit test in `cmd/http/main_test.go`.
- **2026-07-17** — **Transaction-wrap Remember (#222, core P1).** `Remember`'s dedup snapshot,
  `checkNodeQuota`, and `InsertNode` ran as three separate pool statements — non-atomic, so a racing writer
  could slip between the count and the insert and push a namespace over its `max_nodes` quota, and the
  behavior diverged from `Link`, which already counts inside its own tx. Wrapped dedup-check + quota + insert
  in a single `store.WithTx`, so the count Remember sees is the count it inserts against. The summary
  **embed stays outside the tx** (a network round-trip must not hold a transaction open). The idempotent
  create-race recovery (reuse the winner on `ErrNodeExists`, #220) now re-reads *inside* the same tx — safe
  because `InsertNode` uses `ON CONFLICT DO NOTHING RETURNING`, which returns zero rows without aborting the
  transaction. Audit fires only on a real create (unchanged) and after commit. No schema or API change;
  covered in CI by the existing DB-gated create/quota/dedup/idempotency tests (`quota_test.go`,
  `node_identity_test.go`, `audit_test.go`, …).
- **2026-07-17** — **Sentence-overlap chunking (#214, retrieval P1).** The CDC chunker cut with **zero
  overlap**, so a "why" spanning a boundary was halved and neither side won retrieval. `chunk.Split` now
  layers a bounded, sentence-aligned overlap onto the content-defined cores: each chunk after the first is
  prefixed with the previous core's trailing sentence(s), capped at `overlapMax` (256 B) and snapped to a
  sentence terminator (`. ! ?` / newline), falling back to a word break so it never starts mid-word/mid-rune.
  Split was refactored into `splitCores` (unchanged CDC boundaries) + an overlap layer, so the **self-healing
  property is preserved** — the overlap is a function of the previous core's local content, so re-ingest
  still re-embeds only the edited region; the only cost is the blast radius growing by one chunk (the one
  whose overlap changed). Chunk **count is unchanged** (same cores), only text/hashes differ, so the write
  path's hash-reconcile skip/delete logic is untouched — but the first ingest after this ships **re-embeds
  every doc once** (new hashes). Near-duplicate results overlap can produce are already collapsed at
  retrieval (#217), which makes the overlap safe to enable by default. Pure unit tests in
  `internal/chunk/chunk_test.go` (overlap present + suffix-derived, boundary/rune safety, determinism,
  self-heal ≤ 3 chunks). Parent-doc pointer expansion (the issue's alternative) stays a follow-up.
- **2026-07-17** — **Request rate limiting + embed-concurrency cap (#270, security P1).** Storage quotas
  cap rows, not request rate, and the shipped Caddyfile has none — so one token (or open Layer-1 reads)
  could pin the shared Ollama/DB, since **every `/api/search` triggers an embed**. Two independent, opt-in
  controls: **(1) Per-client rate limiting** — a token-bucket middleware on the `/api` group
  (`http.rate_limit_rps` + `rate_limit_burst`, or `HTTP_RATE_LIMIT_RPS`/`_BURST`). A "client" is the
  resolved principal (Layer 2), else a hash of the bearer token, else the source IP — so the limiter sits
  *after* `principalAuth` and never keys on a raw secret. Over-budget requests get `429` + `Retry-After`;
  idle full buckets are pruned ≤ once/min so the map stays bounded under open reads. **(2) Embed-concurrency
  cap** — `embedding.max_concurrency` / `EMBED_MAX_CONCURRENCY` bounds in-flight embed round-trips to Ollama
  via a semaphore *inside* the ollama embedder (`WithMaxConcurrency`), so a bulk ingest plus many concurrent
  searches can't overrun the box regardless of request rate. Both default **off** (0 = unlimited),
  Layer-1/Layer-2 behavior otherwise unchanged. Unit tests (no DB): `rateLimiter.allow` with an injected
  clock, a `429` middleware test, and a peak-in-flight assertion for the embed cap. Rate limiting still
  *also* belongs at the proxy for network-level abuse; this adds the app-level, per-identity half #186 left
  out. Config/auth layer only — no migration.
- **2026-07-16** — **Token lifecycle: entropy floor, hash-at-rest, expiry, hot revocation (#269, security P1).**
  Principal bearer tokens were free-form and long-lived; revocation meant editing config and restarting the
  whole stack (which also kills the MCP server). Five changes, all at the config/auth layer (no DB — tokens
  live in config, not a table): **(1) Entropy floor** — a plaintext principal token must be ≥ `MinTokenLen`
  (32 chars = 128 bits); **(2) `brainiac token gen`** — prints a 256-bit random hex token; **(3) Hash-at-rest**
  — a principal may set `token_sha256:` (the SHA-256 of its token) instead of the secret, so a leaked
  config.yaml holds no live credential; the presented bearer token is hashed and constant-time compared.
  `brainiac token hash` produces the value. Exactly one of `token` / `token_sha256` is required; uniqueness is
  checked on the *hash* so a plaintext token and another's hash of it still collide. **(4) Per-token expiry**
  — an optional RFC3339 `expires:`, enforced per request against the wall clock, so expiry is "hot" (no
  restart). **(5) Hot revocation/rotation** — a `revoked: true` flag plus a `SIGHUP` handler in the HTTP
  server that reloads config and atomically swaps the principal roster (`config.PrincipalAuthenticator`,
  behind an `atomic.Pointer`); a reload that fails validation or empties the roster is rejected, so isolation
  never silently drops. Auth moved from a `map[token]→Principal` matched at boot to a
  `server.PrincipalMatcher` resolving `(token, now)→Principal` per request — the seam that makes expiry and
  hot reload possible. MCP resolves its process principal through the same authenticator (honors
  hash/expiry/revoked at start-up). Unit tests (no DB) in `internal/config/config_test.go`; the entropy floor
  is the only breaking change for existing configs with short tokens. Closes the "token lifecycle" roadmap
  item flagged in [security.md](docs/security.md).
- **2026-07-16** — **Write audit log (#267, security P0).** A company memory of sensitive data must answer
  "who created/changed/deleted X" for compliance and insider-threat review. Added an append-only `audit_log`
  table (migration `0013`) and a best-effort `c.audit(ctx, op, target, namespace)` (swallows its own errors
  so it never fails the write it records) called from every mutation — remember, link, supersede,
  disambiguate, split, merge, delete/handoff namespace, rollup, import. The acting identity is the
  principal's name, or `operator` for an unscoped Layer-1 write. Read surface: `Core.AuditLog` + CLI
  `brainiac audit`. Reads are not audited (higher volume, lower risk — a follow-up); true per-*human*
  identity beyond the per-token principal is also future work.
- **2026-07-16** — **Publish prebuilt images to GHCR (#248).** The app image was compiled from source on the
  target box on every `docker compose up` and every `brainiac update` (pull golang, download modules,
  compile — minutes on weak hardware; rollback compiled twice) — the biggest gap between the "very easy /
  weak hardware" promise and reality. Added `.github/workflows/release.yml`: on a semver tag it builds a
  multi-arch (amd64+arm64) image and pushes `ghcr.io/programmism/brainiac:{version,latest}`. The base
  `docker-compose.yml` now **pulls** `image: ghcr.io/programmism/brainiac:${BRAINIAC_VERSION:-latest}`;
  building from source moved to `docker-compose.dev.yml` (used by CI smoke and local dev). `brainiac update`
  now `docker compose pull`s the versioned image instead of rebuilding, keeping the health-gated rollback.
  **Operational:** the GHCR package must be made public once (repo → Packages) for unauthenticated pulls; the
  first release tag after this change publishes `:latest`.
- **2026-07-16** — **Right-size compose memory to a real 4 GB (#249).** The `mem_limit` caps summed to
  ~4.25 GB (Ollama alone at 3 GB) against the advertised "4 GB box", so the proxy/backup profiles or a
  model pull could OOM. Ollama is sized for the default embedder (nomic, ~1.5 GB peak) at `1536m`; the core
  stack (db 768m + ollama 1536m + app 256m) is now ~2.5 GB, ~2.7 GB with proxy+backup — the "4 GB box" claim
  is honest with headroom. Turning on the local-LLM extractor loads a chat model into the same Ollama and
  needs the cap raised (documented in the compose comment).
- **2026-07-16** — **Health metrics on `/metrics` + index-vs-RAM alert (#255, #256).** The corpus/graph
  health signals (node/edge/chunk counts, edges-per-node, historical/stale %) existed only as on-demand
  JSON on `/api/health`, so Prometheus saw almost nothing alertable. Registered them as gauges
  (`brainiac_nodes_current`, `…_edges_stale`, `…_chunks_hot`, `…_percent_edges_stale`, …) via one
  `c.Health()` query cached ~10s per scrape, plus `brainiac_container_mem_{limit,used}_bytes`. And the ★
  scaling constraint — hot vector index vs RAM (§9) — is now thresholded in `deriveStatus`: warn at 50% of
  the container memory limit, critical at 75%, so the system flags the wall before OOM instead of after.
  Metric wiring lives in the server adapter (`registerHealthGauges`); the threshold lives in the core so
  every client agrees.
- **2026-07-15** — **Enforce the isolation wall on all by-id mutations (#265, security P0).** The Layer-2
  wall was enforced on reads and on name-scoped writes, but the by-id mutations — `Supersede`,
  `Disambiguate`, `Split`, `ApplyMerge`, and proposal `ApproveNode`/`RejectNode` — had **no** principal
  check, so a principal could re-scope, supersede, or merge a node in another namespace by guessing its id
  (cross-namespace takeover). Fix: `assertNodeWritable(ctx, db, id)` loads the node and rejects
  (`ErrForbiddenNamespace`) when `node.project != principal.Write`; every by-id mutation now calls it. Plus a
  principal may not re-scope the **`project`** axis (via disambiguate's `add` or split's `axis`) — that moves
  an entity across the wall. Operators (no principal) are unaffected. DB-gated test in
  `internal/core/mutation_wall_test.go`.
- **2026-07-15** — **nomic task-instruction prefixes (#210, retrieval P0).** `nomic-embed-text` is trained
  for asymmetric retrieval and requires `search_query:` / `search_document:` prefixes; we were embedding
  both queries and documents raw, silently degrading recall corpus-wide and making query/doc cosine
  distances non-comparable. Fix: the Ollama embedder now prefixes documents (`Embed`/`EmbedBatch` →
  `search_document:`) and exposes `EmbedQuery` (→ `search_query:`) via the new `plugins.QueryEmbedder` seam;
  core's search/recall use `embedQuery`. Prefixes default on by model name (`nomic-embed`), overridable via
  `WithTaskPrefixes`, and a symmetric model that doesn't implement `QueryEmbedder` is unaffected.
  **Operational:** existing vectors were stored *without* the document prefix — after upgrading, run
  `brainiac reembed` to rebuild them (raw text is retained, so no re-ingest). The tuned distance cutoffs
  (`MaxRelevantDistance`, `MaxNodeDistance`, `SemanticDupThreshold`) were measured in the un-prefixed regime
  and should be re-calibrated against a real golden set (tracked in #216).
- **2026-07-15** — **Temporal recall — "what did we think about X on date Y" (#200).** Added **valid-time**:
  a `superseded_at timestamptz` on nodes + edges (migration `0008`), stamped `now()` on the flip to
  historical and cleared on a flip back to current — done inside `UpdateNodeStatus`/`UpdateEdgeStatus`, so
  **every** retire path (supersede, merge, split, retire-edge, extractor reject) records when, with no
  per-call-site change. `Core.GetNodeAsOf(id|name, asOf)` returns the entity (nil if it didn't exist by
  asOf) with only edges live at that instant — `created_at <= asOf AND (superseded_at IS NULL OR
  superseded_at > asOf)`, excluding legacy rows retired before valid-time existed (superseded_at NULL +
  historical). The column lives only in WHERE clauses — **not** on the model/DTO — so no scan ripple.
  Surfaced: MCP `get_node` `as_of` (RFC3339 or YYYY-MM-DD), CLI `brainiac node --as-of DATE`. DB-gated test
  in `internal/core/temporal_test.go`. Caveat documented: pre-migration historical rows are invisible to
  as-of views.
- **2026-07-15** — **Rollups: "current state of X" on hub nodes (#198).** Rollup *candidates* (hub nodes with
  many edges) were already surfaced in the Consolidate tab; this adds the rollup itself — a `rollup text`
  column on nodes (migration `0007`), set via `Core.Rollup(nodeID, text)`, distinct from the identity
  `summary` and never touching dedup. A principal may roll up only a node in its own namespace. Surfaced on
  `get_node`, recall node objects, MCP (`nodeDTO.rollup` + a `rollup` tool), CLI (`brainiac rollup <name>
  --text …` and shown in `brainiac node`), and the WebUI entities card. DB-gated tests in
  `internal/core/rollup_test.go`. (Migration numbered `0007` per the convention — one past the highest
  existing prefix.)
- **2026-07-15** — **Namespace import/restore (#196).** Completes the #187 export round-trip: load a
  `brainiac export` JSON bundle back into a namespace. Nodes are upserted via `Remember` (a same-name entity
  already in the target is reused — no forked identity — and summaries are re-embedded); edges reconnect by
  remapping endpoint ids old→new (an edge whose endpoint left the bundle is skipped and counted); chunks are
  re-embedded from their retained text. Target defaults to the bundle's own namespace; a principal may import
  only into its own write namespace. CLI `brainiac namespace import --in FILE [--project X]`. DB-gated tests
  in `internal/core/import_test.go`.
- **2026-07-15** — **Whole-namespace delete + handoff (#188, part of #120 — last follow-up).**
  `DeleteNamespace(project)` removes a namespace's nodes + edges + chunks in one tx (edges touching a
  namespace node go first, to satisfy the FK and because a half-namespace edge is meaningless). An operator
  deletes any namespace; a principal only its own write namespace. `HandoffNamespace(from, to)` re-scopes
  every node + chunk from one project to another (a rename / ownership transfer) — edges follow their
  endpoints by id, untouched, mirroring `UpdateNodeScope`/new `UpdateChunkScope`. Handoff is **operator-only**
  (a principal renaming its own namespace would lock itself out) and **requires an empty target**, so it
  never silently collides two same-named entities into one identity. This avoided the "principal-scoped
  id-based curation" prerequisite by operating at namespace granularity, not per-id — merge/split/supersede
  stay operator-only. CLI `brainiac namespace delete --project X --yes` / `namespace handoff --from X --to Y`.
  DB-gated tests in `internal/core/namespace_test.go`. **Completes the #120 follow-up set** (#186 quotas,
  #187 export, #188 delete/handoff, #189 WebUI auth).
- **2026-07-15** — **Per-namespace storage quotas (#186, part of #120).** A principal's namespace can cap
  its row counts: `max_nodes` / `max_chunks` on the principal (0 = unlimited). Enforced in the core at write
  time against the live count — `checkNodeQuota` before a *new* node in remember/link (so a link between two
  existing nodes at cap still succeeds, only new endpoints count), `checkChunkQuota` in ingest *after* the
  stale-chunk delete (a re-ingest that replaces content isn't wrongly rejected). Over-cap writes return
  `ErrQuotaExceeded`. No principal or a 0 cap = unlimited (Layer 1 unchanged). **Rate limiting** (requests/s)
  is deliberately out of scope here — it belongs at the proxy/middleware layer, orthogonal to the storage
  quota; #186 covers the row-count half. DB-gated tests in `internal/core/quota_test.go`.
- **2026-07-15** — **Per-namespace export (#187, part of #120).** A namespace backup/hand-off:
  `Core.ExportNamespace(project)` dumps all nodes + edges + chunks in one project as portable JSON, reusing
  the Layer 2 wall predicate as the `WHERE` (`store.Export{Nodes,Edges,Chunks}`). Edges follow the same
  **both-endpoints** rule as reads, so an export never carries a dangling reference outside the namespace.
  Embeddings are omitted — recomputable from the retained raw text on import (§7), keeping the JSON small and
  model-agnostic. Access: an operator (Layer 1) exports any namespace by name; a principal only one in its
  read-set (a foreign name → `ErrForbiddenNamespace`, never a silent empty). Surfaced as CLI
  `brainiac export --project X [--out FILE]`. Import/restore is a separate follow-up.
- **2026-07-15** — **WebUI auth under hard isolation (#189, part of #120).** With principals on, every
  `/api` read is walled, but the WebUI only sent its token on *writes* — so the search/graph tabs 401'd and
  the token bar only appeared in write mode. Fix: `getJSON` now sends `Authorization: Bearer <token>` on
  reads too (a harmless ignored header in Layer 1); `/api/capabilities` stays **public** even under isolation
  (booleans only, no memory data — `principalAuth` allow-lists it) and gained `auth_required`, so the WebUI
  shows the token bar and prompts for a token before it has one, then refreshes the active tab once saved.
  Thin-adapter change only.
- **2026-07-15** — **Migration numbering convention: next number = highest existing prefix + 1; never
  reuse a prefix.** `0006_node_summary.sql` shipped alongside the pre-existing `0006_proposed_status.sql`
  — two `0006_*` files. It *runs* (Migrate keys `schema_migrations` by full filename, and the two alter
  independent objects), but a reused number is confusing and, worse, **unrenamable after shipping**: on
  any DB that already recorded the file, renaming it makes Migrate re-run its `ADD COLUMN` and fail
  "already exists". Concrete bite: a DB that had applied a superseded branch's `0007_node_summary.sql`
  crash-looped on boot when main's `0006_node_summary.sql` tried to re-add the column; fixed by hand
  (record `0006_node_summary` as applied, drop the stale `0007` row). **Rule going forward:** before
  adding a migration, `ls internal/store/migrations/` and pick one past the highest numeric prefix — never
  reuse a number, even across parallel branches (rebase/renumber before merge if two branches both grab N).
- **2026-07-15** — **Layer 2 hard isolation — foundation (#185, part of #120).** Turned "hard isolation = a
  separate stack per team" into **one server, many namespaces, per-token wall** — the only shape that adds
  value over running a second stack (a shared graph + shared/global layer behind a per-caller wall). Model:
  a bearer token → a `Principal{Read: []namespace, Write: namespace}`; namespace = the `project`
  discriminator value (**no migration** — reuses the Layer 1 identity columns). Decisions: (1) enforcement
  lives in the **core**, not an HTTP filter — a `Principal` rides on `context.Context` (`core.WithPrincipal`,
  set by HTTP `principalAuth` per request and by the MCP process from config), and `readScope`/`pinWrite` are
  the single choke points; a compiler-forced `store.Wall` on every multi-row read (`SearchChunks`,
  `FindNodesByMention`, `FindSimilarNodes`, `EdgesForNode`, `GraphSnapshot`, `GetChunksBySourceURI`) means a
  future read can't silently skip the wall. (2) Wall predicate is on `discriminators->>'project'` (not the
  exact `scope_key`) so it walls multi-axis rows (`project=A;env=prod`) correctly; `$wall IS NULL` is the
  Layer-1 sentinel. (3) **Edges** have no scope column → an edge is visible only when **both** endpoints are
  in the wall, so a cross-namespace edge is hidden. (4) Single-node `get_node` lookups post-filter in core
  (row fetched but withheld) so an id/name guess across the wall reads as "not found", never a leak. (5)
  **Secure global default** — a namespace-scoped principal sees global only if its read-set explicitly lists
  it. (6) **Write-pin** — remember/link/ingest force the `project` axis to the principal's single write
  target; a caller naming another namespace is rejected (`ErrForbiddenNamespace`), not silently redirected.
  (7) Under isolation the HTTP surface requires a principal token for **reads too**, and the operator-only
  id-based curation write group is not mounted (crosses namespaces; deferred to #188). Off by default: no
  `principals:` configured ⇒ Layer 1, byte-identical (proven by `TestNilPrincipalIsLayer1`). DB-gated tests
  in `internal/core/isolation_test.go`. Follow-ups: #186 quotas, #187 export, #188 delete/handoff, #189 WebUI
  read-auth.
- **2026-07-14** — **Persist node summary text (Tier 3 of #181 — closes it).** A node's description was
  embedded into `summary_embedding` and the prose thrown away, so "describe X" had no text to return and a
  node could not cite itself; the only human-readable fields were name/aliases/type. Fix: a `summary text`
  column (migration `0006`) stored on `remember` alongside the derived embedding, carried on `model.Node`
  (JSON `summary`) through every read (`nodeCols` + `scanNode`), and surfaced on all clients — MCP
  `nodeDTO.summary` (both `recall` and `get_node`), `GET /api/recall`/`/api/node` (raw node JSON), CLI
  `recall`/`node`, WebUI entities card. **Backfill:** re-remembering with a description updates the text
  **and** re-embeds it together (`store.UpdateNodeSummary`), so prose and vector never drift; nodes created
  before this stay NULL until re-remembered. Completes the tiered #181 (Tier 1 rich node objects, Tier 2
  `get_node`, Tier 3 summary text).
- **2026-07-14** — **`get_node` — direct entity lookup (Tier 2 of #181).** Added a by-name (project-scoped,
  then global) or by-id read that returns a node's full record (aliases, type, discriminators, status)
  **plus its edges** — the "I already know this entity; give me its details and relationships" path that
  `recall` (semantic discovery) didn't serve. `Core.GetNode` wraps existing store reads
  (`GetNodeByCanonicalNameScoped` / `GetNodeByID` + `EdgesForNode`); exposed as the MCP `get_node` tool,
  `GET /api/node?name=|id=[&project=]` (404 on miss), and CLI `brainiac node <name> [--id] [--project]`.
  Complements Tier 1. Remaining: persist node summary **text** (Tier 3, #181).
- **2026-07-14** — **Recall returns node objects, not bare names (Tier 1 of #181).** `recall` surfaced
  entity nodes as `canonical_name` strings on the agent/human clients (MCP `recallOut.Nodes []string`,
  CLI `nodeNames`, WebUI), discarding aliases/type/discriminators/status even though the core carries
  full `[]model.Node` — so a caller that recalled an entity couldn't read its aliases without a direct
  Postgres query. Fix: a `nodeDTO {id, canonical_name, aliases, type, discriminators, status}` (mirrors
  `edgeDTO`) is now emitted from MCP `recall`; the CLI renders `name [type] (aka: …)`; the WebUI shows
  type + aliases in the entities card. HTTP `/api/recall` already returned full node JSON (unchanged).
  Chunk/edge output untouched. Follow-ups tracked in #181: a `get_node` by-id/name lookup (Tier 2) and
  persisting node summary **text** — today only its embedding is stored (Tier 3).
- **2026-07-14** — **Recall node precision + lexical matching.** `recall` retrieved entity nodes by
  `summary_embedding` proximity only, with the chunk-tuned cutoff (`MaxRelevantDistance = 0.75`) and
  `DefaultRecallNodes = 5`. On a corpus dominated by one domain, a query about a sparse entity admitted
  ~5 weakly-similar nodes; their edges (traversed for *every* admitted node, cap 100) then flooded the
  bundle with off-topic rationale. Two root gaps: (1) names/aliases are **not embedded** (only a node's
  `Summary` is), so a query that literally names an entity had no vector path to it; (2) the cutoff was
  too lenient for short node summaries and edges were taken from all admitted nodes without a relevance
  order. Fix (`recall.go`, `store.FindNodesByMention`): a **lexical path** admits nodes whose
  `canonical_name` or any alias occurs as a whole word/phrase in the query (distance-independent, ranked
  first), plus a **precision path** for vector hits — a dedicated node cutoff `MaxNodeDistance = 0.55`
  (distinct from the chunk 0.75) **and** a relative gap `NodeDistanceGap = 0.10` from the best hit, with
  `DefaultRecallNodes` 5→3 and `maxRecallEdges` 100→40, edges traversed in relevance order. Grounded in
  measured nomic-embed distances (a named entity sits well under 0.5 vs unrelated nodes >0.55; a foreign
  query bottoms out ~0.59). Result: a query naming an entity returns that entity + its own edges/aliases;
  a foreign query returns empty. Chunk search (`search.go`) is untouched. DB-gated tests added
  (`recall_test.go`); the chunk-based eval harness (#29) does not cover node recall — a node-recall metric
  is a follow-up.
- **2026-07-14** — Wrapper help no longer says "kb" (#178): running `brainiac` (bare) or `brainiac help`
  forwarded to the in-container CLI, whose cobra help is hardcoded to `Use: "kb"` — so the help talked
  about `kb` even though the user invoked `brainiac`. Two fixes: (1) the wrapper now has its **own** `help`
  (also `-h`/`--help`, and the bare invocation) listing the wrapper verbs plus the forwarded CLI verbs;
  (2) the CLI's root command name is read from **`BRAINIAC_CLI_NAME`** (default `kb`), and the wrapper
  forwards CLI commands with `BRAINIAC_CLI_NAME=brainiac`, so sub-command help renders `brainiac search …`
  when invoked through the wrapper while a direct `/kb` in the container stays `kb`. Verified both paths;
  shell `sh -n` clean, CLI test covers the env-driven name. (#178)

- **2026-07-14** — `brainiac install` puts the wrapper on PATH (#176): the `brainiac` helper was runnable
  only as `./brainiac` from the checkout. Added `install [dir]` / `uninstall [dir]` subcommands that
  symlink it onto PATH (default `/usr/local/bin`, with PATH + writability checks and a sudo / `~/.local/bin`
  fallback hint). The script now **resolves its own real path through symlinks** before `cd`-ing to the
  checkout (a bare symlink would otherwise `cd` to `/usr/local/bin` and miss compose/.env/git). Because the
  install is a symlink to the checkout copy, `brainiac update` (git checkout in place) keeps the installed
  command current with no reinstall and no per-update system writes. Shell-only, `sh -n` clean; verified the
  symlinked invocation resolves back to the real checkout. (#176)

- **2026-07-13** — `WEBUI_MODE` env override (#174): WebUI writes could not be enabled in a standard
  `docker compose` deploy at all — `clients.webui` was settable only via `config.yaml`, but the shipped
  image carries **no config.yaml** (config-less → `Default()` → `read-only`) and compose mounts none. So
  every write button 404'd (`POST /api/edges/{id}/retire` → the unmounted-route JSON 404 from #168), with
  no switch to flip. Added a `WEBUI_MODE` (`read-only`|`interactive`) env override in `applyEnvOverrides`,
  passed through in compose; `WEBUI_MODE=interactive` + `AUTH_TOKEN` now enables writes with no config
  file — consistent with `DATABASE_URL`/`AUTH_TOKEN`/`EXTRACTOR`. `.env.example` documents both are
  required; config test covers the override. Secure by default unchanged (unset = read-only). (#174)
- **2026-07-13** — Update health gate waits on container health, not a fixed curl window: a real
  v1.29.0→v1.30.2 update tripped a **false rollback** — the version was sound (a plain `up -d --build app`
  came up healthy in seconds), but during the coordinated full-stack recreate the app didn't answer
  `/readyz` within the fixed 120s (60×2s) curl window. Fix: the gate now polls the **app container's own
  Docker health status** (`docker inspect .State.Health.Status`, the same `/brainiac healthcheck` Docker
  uses), waits up to 180s (90×2s), treats `starting` as keep-waiting, breaks early on `unhealthy`, and
  succeeds on `healthy`; it falls back to curl `/readyz` only when `app` declares no healthcheck. This
  gates on the authoritative signal, so a slow first boot no longer reads as failure. Shell-only change to
  the `brainiac` wrapper (`sh -n` clean); no binary/API/schema impact.
- **2026-07-09** — WebUI write actions fixed (#168): a conflict-resolution (and any other write) button
  threw a cryptic "invalid JSON" error. Root cause chain: default `clients.webui=read-only` ⇒ write
  endpoints unmounted; the WebUI rendered action buttons regardless; clicking POSTed to an unmounted route ⇒
  chi's plain-text 404 ⇒ the WebUI's `post()` blindly ran `r.json()` ⇒ parse error. (Even interactive would
  have 401'd — `post()` sent no bearer token.) Fix: (1) `/api` now returns **JSON** for NotFound/
  MethodNotAllowed so the API is uniformly JSON; (2) `GET /api/capabilities` (`{writable}`, no DB) lets the
  UI gate its controls — read-only shows a hint instead of dead buttons; (3) a **token field** in the UI
  (paste `AUTH_TOKEN` → localStorage → `Authorization: Bearer` on writes), chosen over embedding the secret
  in the page or trusting the proxy, keeping secure-by-default; (4) a hardened `post()` that surfaces
  non-2xx/non-JSON as a readable message (401 → "paste the AUTH_TOKEN"). Non-DB server tests for
  capabilities + the JSON-404 guarantee. To actually act in the WebUI: set `clients.webui=interactive` +
  `AUTH_TOKEN`, then paste that token in the UI. (#168)
- **2026-07-08** — In-app log capture + WebUI Logs tab (#166): motivated by a WebUI conflict-resolution
  error that was invisible without container shell access. The HTTP process tees the standard logger **and**
  the chi access log (via a replaced `accessLogger` middleware) through `io.MultiWriter(stderr, ring)`, where
  the ring is a bounded, thread-safe `internal/logbuf.Buffer` (last ~2000 lines, in-memory, no disk).
  `GET /api/logs` (open-read, same posture as `/system`; mounted only when the sink is set) serves them, and
  a WebUI **Logs** tab renders them newest-last with per-line + "Copy all" copy buttons and optional
  auto-refresh. **Secrets are redacted at capture** (`Redact`: PATs, `Bearer …`, `token=…`) so the viewer
  can't leak a credential — a deliberate constraint since access logs carry request paths/queries. Chose
  in-memory ring over a file/log-service to preserve the single-container, no-extra-infra ethos. Unit tests
  for the buffer (ring/partial-line/redaction/concurrent) + a non-DB server test for `/api/logs` incl. the
  redaction guarantee. Follow-up: use the tab to capture the real conflict error and fix that bug. (#166)
- **2026-07-08** — Pluggable local-LLM extractor (#164): made the `Extractor` seam real. An optional
  Ollama chat model (`extraction.default: local-llm` / `EXTRACTOR=local-llm`, model required) turns
  ingested hot chunks into nodes/edges via structured outputs (`/api/chat` + JSON schema), so a beefy
  self-hosted box can auto-extract while a weak box keeps the free chat-driven path (default, unchanged).
  Runs best-effort per chunk after the chunk reconcile (chunks are provenance and must persist even if
  extraction fails). Chose a **review-by-default gate** over direct writes — a local model is weaker than
  Claude — implemented as a third status **`proposed`**: extracted rows are invisible to every read (all
  reads already filter `status='current'`) until a human approves them (WebUI Proposals tab, MCP
  `proposals`/`review_proposal`, `GET/POST /api/proposals/...`). `extraction.review: false` writes live
  `current` for a trusted model. Provenance `author=local-llm`. Nodes carry no unique constraint so
  approval never collides; an approved edge promotes its endpoints and retires itself if a current edge
  already covers the triple. Unit tests for the extractor (httptest); DB-gated core tests for the
  propose→approve/dedup/review-off flows. Partially answers the §"open questions" item on a local
  consolidation LLM — the *extraction* half is now local-optional; consolidation stays as-is. (#164)
- **2026-07-07** — Repo hygiene (#162): dropped a stray ~18 MB `mcp` binary committed by accident
  (1df475c) — it was the whole repo size, unused (Docker builds from source), no secrets (only Go build
  paths). Hardened `.gitignore` to exclude bare root-level build artifacts (`/mcp`, `/http`, `/cli`, `/kb`,
  …) so `go build ./cmd/...` in the root can't sneak one in again (official builds go to `/bin` or `/out`).
  History purge (for size) is a separate optional step. Prep for making the repo public. (#162)
- **2026-07-07** — One-command health-gated update (#160): updating was manual (`git pull && docker compose
  up -d --build`). Added `./brainiac update` (POSIX-sh wrapper case): refuse on a dirty tree → `git fetch
  --tags` → pick the latest semver **release tag** (not `main` — never auto-deploy untested code) → checkout
  + `docker compose up -d --build` → poll `/readyz` (default `127.0.0.1:${HTTP_PORT:-8080}`, 60×2s) → **roll
  back to the prior ref + rebuild on build failure or unhealthy**. Already-on-latest is a no-op; missing
  `curl` skips the health gate with a warning. Scheduling is a **documented host cron/systemd recipe**, not
  an in-app daemon — the runtime data (volumes) is untouched and migrations auto-apply, so the whole thing
  stays a shell operation. README "Updating" rewritten. Not exercised by Go CI (shell); `sh -n`/`bash -n`
  syntax-checked and logic-reviewed; the runtime path needs Docker (absent in the dev sandbox). (#160)
- **2026-07-07** — Ingest progress reporting (#139): a long import was a black box — only a final summary
  line, nothing until the doc finished. Added `IngestOptions.OnProgress func(IngestProgress)`, emitted from
  core between embed batches (`IngestProgress{Doc, Embedded, ToEmbed, Stats}`). Kept **opt-in and behind the
  existing seam**: with no callback, embedding stays one shot (the #140 batch path unchanged); with one,
  `embedWithProgress` steps through the chunks in `ingestProgressStep` (64) batches and reports between. CLI
  renders `embedding <doc>: X/Y chunks` to stderr (and a final newline); auto-import logs only large-doc
  (≥64 chunks) completion so small markdown files stay quiet. Denominator is real because chunking + density
  selection happen before embedding, so `ToEmbed` is known up front. DB-gated test: a many-chunk doc yields
  ≥2 callbacks, non-decreasing `Embedded`, final `Embedded == ToEmbed`. (#139)
- **2026-07-07** — Dry-run import (#142): `kb import --dry-run` (`IngestOptions.DryRun`) runs chunk +
  density selection but embeds nothing and writes nothing, returning `IngestStats` for what *would* happen —
  chunk count, kept/queued/dropped/skipped, and a would-delete count (stored chunks whose hash is no longer
  present, computed in-memory from the existing-hash set, no destructive call). Lets you preview the size of
  a big import and see what the density filter keeps vs drops before committing, and catch a wrongly-scoped
  import for free. CLI labels the output "dry run (nothing written)". `ingestDoc` now takes `IngestOptions`
  (so dry-run and future per-run knobs thread through). DB-gated test: dry run writes 0 chunks and its
  kept/queued/dropped exactly match a subsequent real ingest. (#142)
- **2026-07-07** — Type normalization + seed vocabulary (#156, phase 1): node/edge `type` was free text, so
  separator/case variants of one intent (`writes_to` / `writes-to` / `writesTo`) became **distinct types** —
  fragmenting the graph and, worse, letting conflict detection (same `from`+`type` → different targets)
  silently miss contradictions written differently. Added `core.normalizeType` (camelCase→snake, any run of
  space/`.`/`-`/`_` → single `_`, lowered, trimmed; pure + idempotent), applied on `link`/`remember`. It
  folds **case + separators only** — never synonyms, so it can't merge two genuinely-distinct types
  (`publishes_to` stays distinct). Chose soft normalization + a **non-binding seed vocabulary** (in the agent
  instruction block, MCP tool schemas, and `concepts-and-workflows.md`) over a hard vocabulary, which would
  fight Brainiac's domain-neutrality. New-writes-only (no retro migration of existing rows). Tests: unit
  (`normalizeType` cases + idempotency) + DB-gated (two edges typed `writes-to` and `writesTo` now surface as
  one conflict). Alias maps (`publishes_to`→`writes_to`) remain a possible later step; see #156. (#156)
- **2026-07-07** — Consolidate tab inline help (#153): the WebUI consolidation queue rendered five
  sections (merge / conflicts / splits / stale / rollups) with no explanation, so a new operator had to
  guess what a "keep « X »" or "Confirm" button meant. Added a one-line muted explainer under each section
  header (what it is + what the action does). Static helper text, no deps, single-file WebUI style
  preserved. Pairs with the concepts guide (#152). (#153)
- **2026-07-07** — Concepts & Workflows guide (#152): reference docs (MCP tool descriptions, CLI help,
  README) were good but there was no *task-oriented* guide — the curation verbs
  (`consolidate/merge/split/retire-edge/confirm/disambiguate/supersede`) were scattered, worst for the
  CLI/WebUI operator hitting the consolidation queue cold. Added `docs/concepts-and-workflows.md`: the
  two-layer mental model, a verb cheat-sheet (what/when), the consolidation queue decoded, and a
  scenario→action table. Linked from README. Docs-only; the WebUI inline-help counterpart is #153. (#152)
- **2026-07-07** — Actionable graph conflicts (#148): `FindConflicts` already detected contradictions
  (same `from`+`type`, different targets) but they were a **dead end** — read-only in the WebUI, and the
  only resolution path (`supersede`) operates on *nodes*, which is wrong for an edge conflict (it would mark
  a target node historical, corrupting its use elsewhere). Added edge-level resolution: `store.UpdateEdgeStatus`
  + `Core.RetireEdge(edgeID)` mark the losing edge `historical` (mirror of node supersession; recall still
  reaches it via history — replacement, not deletion). Conflicts now carry both edge ids (`FindConflicts`
  selects `e1.id, e2.id`; `Conflict` gains `EdgeA`/`EdgeB`). Surfaced: WebUI "keep « X »" buttons per side +
  a rendered **Split candidates** section (was never shown), `POST /api/edges/{id}/retire`, CLI
  `kb retire-edge <id>` and edge ids in the `conflicts` output. Still propose-not-apply: detection automatic,
  retire an explicit human action. DB-gated test: two conflicting edges → retire one → conflict gone, loser
  historical + reachable via history, missing id errors. (#148)
- **2026-07-07** — Recency-based staleness detection (#147): SYSTEM.md §8.3 specified the only *automatic*
  "this might be outdated" signal — flag an edge when `source_modified_at > edge.created_at` — but it was
  never implemented; `flagged_stale` was set purely by hand. Added `store.FlagStaleBySource`: Consolidate
  now flags current edges whose source has a chunk with `source_modified_at > COALESCE(last_confirmed_at,
  created_at)`. Comparing against `last_confirmed_at` (not just `created_at`) means a confirmed edge isn't
  re-flagged until the source changes again — no re-flag loop. It reuses the existing Stale list + Confirm
  action, so no new UI. This is the **one write** the librarian pass makes, and it's review-only (reversible
  via Confirm, never alters meaning or supersedes) — consistent with "propose, not apply". Closes the
  spec/code drift. DB-gated test: aged edge + newer source → flagged; Confirm → not re-flagged; source
  changes again → re-flagged. (#147)
- **2026-07-07** — Whitespace normalization before chunking (#146): ingest had **no text preprocessing** —
  `chunk.Split` trims only chunk edges, so interior formatting (e.g. a conversion that put a blank line
  between every line) was embedded and stored verbatim (ugly evidence, byte-size inflation, and "sticky":
  cleaning the source later re-hashes every chunk → full re-embed). Added `core.normalizeText`, applied once
  in `ingestDoc` **before** `Split`: CRLF→LF, per-line trailing spaces/tabs stripped, blank-line runs
  collapsed to one, whole text trimmed. Pure + idempotent, so chunk boundaries stay content-defined and
  self-healing is preserved; touches formatting only, never drops content words (density scoring unaffected).
  One-time effect: the first ingest after this ships re-hashes existing docs. Unit tests (normalization cases
  + idempotency). (#146)
- **2026-07-06** — Batch embedding on ingest (#140): ingest embedded **one chunk per HTTP round-trip,
  serially** — the dominant cost of a bulk import (a ~1,100-chunk book pegged Ollama for minutes while the
  app sat blocked on each call). Added an optional `plugins.BatchEmbedder` seam (`EmbedBatch([]string)`);
  `Ingest` now runs **two passes** — decide skip/drop/keep, then embed all survivors via the batch path when
  the embedder exposes it (falls back to per-chunk `Embed` otherwise, so the core stays transparent). The
  Ollama embedder implements it against **`/api/embed`** (array `input`), sending `embedding.batch_size`
  (default 32) chunks per request — chosen over client-side concurrency to avoid swamping a 4 GB prototype
  box; batch size is tunable for boxes with more headroom. `ingestDoc` still reconciles per document in one
  short transaction (invariant unchanged; embeddings computed before the tx). Tests: Ollama `EmbedBatch`
  (batching/alignment/error via httptest), core `embedTexts` (batch-vs-fallback, alignment), and the DB-gated
  ingest suite now runs through the batch path (`hashEmbedder` implements `BatchEmbedder`). (#140)
- **2026-07-06** — Recall/search scope provenance (#143): the soft lens (#119) returns project **+ global**,
  so a query scoped to an empty project silently gets **global** results that read as if they belong to the
  project. Made scope legible without changing the lens semantics: every search/recall hit now carries a
  `scope` label (`model.ScopeLabel` → `"global"` | `"project:NAME"` | raw scope_key), derived from each
  chunk/node's discriminators — `SearchChunks` now selects `discriminators` so chunk hits carry it like nodes
  already did. `RecallResult` gained `Scope` (the requested scope) + `ScopeFallback`, set true when a scoped
  query found nothing in-project and **every** returned result is global (the "0 in this project; showing
  global" signal). Surfaced everywhere: WebUI scope badges + a fallback banner, CLI scope tags (project-only,
  global stays unmarked) + a fallback line, MCP `scope` per chunk + `scope_fallback` in the recall summary.
  Unit test (`ScopeLabel`) + DB-gated test (alpha hit=`project:alpha`, global=`global`; recall vs empty
  project flags fallback, recall vs populated project does not). (#143)
- **2026-07-04** — Tangled-node split + detector (#127): the mirror of merge, completing reactive
  disambiguation. **Detector** (`store.ProposeNodeSplits`, surfaced in the Consolidate report as `Splits`):
  flags nodes whose current edges contradict (same `from`+`type`, ≥2 targets) — a likely conflation.
  **Op** `Split(nodeID, axis, routes)` (CLI `kb split`, `POST /api/split`; not MCP — edge-id routing is
  review UX like `merge`): carves the node into scoped children `{axis:value}`, repoints each routed edge to
  its child (collision-safe via `store.RepointEdgeEndpoint` — a colliding edge is retired, not duplicated),
  and retires the parent if no current edges remain. Handles the case `disambiguate` (#126) can't: facts
  belonging to *different* values, not the whole node. Decisions: children get **no** summary_embedding
  (the parent's was the conflated one — misleading; re-summarize later); detector uses the contradictory-edge
  signal only (the "two source clusters" signal is deferred, fuzzier). DB test: contradictory Config →
  candidate → split into env=prod/env=staging children (one edge each), parent retired. (#127)
- **2026-07-04** — System metrics panel (#132): new read-only **`GET /api/system`** + a **System** tab in
  the WebUI surfacing *operational* health, separate from the corpus "Health" tab — so an operator sees when
  the deployment nears its allocated-resource ceiling. Logic in `core.SystemMetrics` (thin server + webui
  adapters, per the core rule): container cgroup memory (new dep-free `internal/sysstat`, cgroup v2→v1,
  best-effort — `available:false` off-Linux so dev on macOS degrades cleanly), DB size + connection/pool
  saturation (`store.DBStatsFor` + `pgxpool.Stat`), and the Go process footprint. A `status`
  (`ok`/`warn`/`critical`) with warnings rolls up from thresholds kept in core (mem 85/95%, conn & pool 80%),
  so every client shows the same verdict. `Core` gained a `startedAt` for uptime. Point-in-time only — history
  stays with the Prometheus `/metrics` scrape. Tests: DB-free `deriveStatus` table + `sysstat` graceful-off-
  Linux, DB-gated `SystemMetrics` and `/api/system` end-to-end. (#132)
- **2026-07-03** — Reactive disambiguation (#126): new op **`Disambiguate(nodeID, axes)`** (MCP tool +
  `kb disambiguate` CLI) — the reactive way to configure discriminators. When you notice one entity conflates
  two things, you add the axis that separates them (`env=prod`) onto the existing node; its scope_key is
  rewritten in place and its edges/facts move with it (they reference it by id — no routing). A later save of
  the other variant becomes a distinct entity. Guard: if a current node already occupies the target
  `(name, scope)`, it errors and points to `merge` (never silently folds). It's the mirror of Consolidate's
  merge (merge collapses wrong duplicates; disambiguate separates a wrongly-conflated one). Store gained
  `UpdateNodeScope`. Instruction block + tools list teach the move ("introduce an axis only when you see a
  conflation, don't over-tag"). Splitting a genuinely *tangled* node (facts belonging to different values)
  needs per-edge routing + a librarian detector — deferred to #127. DB test: re-scope preserves id+edges,
  old scope emptied, staging variant distinct, collision errors. (#126)
- **2026-07-03** — Generic discriminators (#125): capture accepts **arbitrary identity axes**, not just
  `project` — `remember`/`link` take a `discriminators` map (MCP) / repeatable `--disc key=value` (CLI),
  merged with `project` (sugar; the flag wins on conflict) via `core.Discriminators`. `model.ValidateDiscriminators`
  rejects empty or `;`/`=`-bearing keys/values (they'd corrupt the `k=v;` `scope_key` and let a crafted single
  pair collide with a multi-pair set). Enables finer identity (`env=prod` vs `env=staging`) and unlocks the
  **reactive** model (#126): live on `project`, introduce an axis only when you actually see a conflation —
  no upfront vocabulary. DB-free tests (scope_key order-independence, validation, merge) + DB test (env axis
  yields a distinct node; invalid disc rejected). (#125)
- **2026-07-03** — Soft retrieval lens (#119, part of #113): `Search`/`Recall` (MCP + CLI + HTTP) gained an
  optional **`project`** — when set, retrieval is scoped to that project **+ global** over *both* chunks and
  nodes; when omitted, it spans all scopes (cross-project search, unchanged default). Chose **default-scoped-
  when-project-known**: behavior only narrows when the caller opts in by naming a project, so nothing breaks
  for callers that don't. Chunks gained `discriminators`/`scope_key` (migration 0005) so the lens covers
  documents, not just the graph; `add_document`/`ingest`/`import` accept `project` to tag chunks.
  `store.ScopeFilter` (empty = all scopes; `LensFor(project)` = {project, global}; `ExactScope` for dedup)
  unifies node + chunk scoping. Instruction block tells agents to pass `project` on recall/search. It's a
  **soft** lens — nothing hidden, widen by omitting the project; hard isolation is still #120. DB-gated test:
  same text under alpha/beta/global → alpha lens returns alpha+global not beta; no-project spans all. (#119)
- **2026-07-02** — Consolidate respects identity scope (#118, part of #113): `ProposeNodeMerges` now groups
  duplicate-name candidates by `(scope_key, normalized_name)` instead of name alone, so Consolidate never
  proposes merging same-named entities across projects — closing the loop opened by #117/#116 (otherwise the
  librarian pass would re-merge what scoped identity kept apart). Same-project duplicates still group as
  before. DB-gated test: `OrderService{alpha}` vs `Order Service{beta}` → no proposal; two `Pay Service` in
  the same project → one group. (#118)
- **2026-07-02** — Project tagging at capture (#116, part of #113): `remember`/`link` (MCP + CLI) gained
  an optional **`project`** — the agent, which knows its working context, passes the project it's in, and it
  becomes the identity discriminator `{project: …}` (empty = global). Decided **agent-passes-in-call** over a
  per-MCP-registration default: one Brainiac serves many projects/agents, and the memory instruction already
  nudges the agent to tag saves. `Link` now resolves/creates **both endpoints within that scope** (scoped
  `ensureNode`). Universal facts (a vendor, a standard) omit `project` and stay global. Instruction block +
  `./brainiac instructions` updated to tell agents to tag by project. This activates the #117 machinery;
  richer multi-axis discriminators (env/client) remain supported at the core level. DB-gated MCP test: same
  name + different `project` → distinct nodes; same project → idempotent. (#116)
- **2026-07-02** — Scoped node identity (#117, part of #113): a node's identity is now `canonical_name` +
  a **discriminator** set (identity axes like `project`/`env`; empty = **global/shared**), not name alone.
  Migration 0004 adds `discriminators jsonb` + a canonical `scope_key` (sorted `k=v;`, app-written); upsert
  and dedup (`GetNodeByCanonicalNameScoped`, `FindNodesByNormalizedName`, `FindSimilarNodes` all scope-keyed)
  key on `(scope_key, canonical_name)`. Effect: same-named entities in different projects stay distinct and
  accrue their own facts; universal entities (empty discriminators) accrue globally. **Backward-compatible**:
  discriminators default to `{}`/global, so existing nodes and callers behave exactly as before until #116
  auto-populates `project` from context. Recall reads across all scopes (`store.AnyScope`) — the soft
  per-project lens is #119; Consolidate scoping is #118. DB-gated test: two `Config{project:…}` stay distinct,
  same-scope re-remember is idempotent, global `Config` is a third identity. (#117)
- **2026-07-02** — Agent-memory docs (#111 follow-up): documented how to make Brainiac **any MCP agent's
  long-term memory** — connect the stdio MCP server (Claude Desktop/Code, Cursor, Cline, custom SDK) and
  paste an agent-agnostic **memory instruction** (recall-before-answering + save findings/decisions
  proactively) globally or per-project. New `./brainiac instructions` prints that block; `docs/agent-memory.md`
  covers connect + instruct + global-vs-per-project. **Per-project scoping is a soft convention today**
  (source_uri prefixes + a `project:` note); **hard isolation = namespaces is future** (#113, PRD §21) —
  until then run a separate stack per project.
- **2026-07-02** — Decided: **content is retained after a source file is deleted** (#107 closed, not
  built). Editing reconciles a source; deletion does not prune it — a memory persists even if the source
  is gone. Documented in §7; drop content via `docker compose down -v` or targeted DB deletes.
- **2026-07-02** — MCP `add_document` (#111): `add_document{source_uri, text}` → `core.IngestText` runs
  supplied text through the pipeline (chunk→select→embed→store, per-source reconcile). Enables the
  **chat-driven** path: Claude reads a source with **its own** integration (Notion, web) and pushes the
  text into the searchable memory — no `NOTION_TOKEN`/connector needed for ad-hoc imports. `remember`/`link`
  still build the graph; the connector (`ingest`) remains for bulk/scheduled. DB-gated tests
  (`IngestText` + MCP add_document→search). (#111)
- **2026-07-02** — MCP `ingest` tool (#108): Claude can now drive imports — `ingest{source, target}`
  (source notion|markdown; target a Notion page URL/id or path; empty = whole source). The Notion
  connector gained single-page fetch (`NewForPages` + `ParsePageID`, `GET /v1/pages/{id}`), so *"import
  this Notion link"* imports just that page. `mcpserver.New(core, ImportFunc)` takes an app-supplied
  dispatcher (keeps core/mcp plugin-agnostic); `cmd/mcp` builds it from config. CLI parity:
  `kb import --source notion --path <url>`. Tests: ParsePageID, single-page fetch, MCP ingest round-trip. (#108)
- **2026-07-02** — Notion works out of the box (#105): compose now passes `NOTION_TOKEN` (and
  `INGEST_INTERVAL` — which was missing, so auto-import never fired in Docker) into the app; setting
  `NOTION_TOKEN` alone **auto-creates** a notion source (no config.yaml needed), so `./brainiac import
  --source notion` works. Notion is on-demand/cron (not in the per-minute local auto-import loop) to
  avoid hammering the API. Notion + Markdown share one corpus. Laptop guide gets a Notion section. (#105)
- **2026-07-01** — Simpler laptop UX (#103): a `./brainiac` wrapper turns `docker compose exec app /kb …`
  into `./brainiac …` (+ `up`/`down`/`logs`/`mcp-config`, and `import` defaults to `/data/docs`). Optional
  **auto-import**: `INGEST_INTERVAL` (env/`ingest.interval`) runs a background loop re-ingesting
  `/data/docs` + configured markdown sources on a timer (cheap via content-hash reconcile + CDC) — drop
  files, they appear. Markdown connector tolerates a missing dir. `.env.example` ships `INGEST_INTERVAL=60s`. (#103)
- **2026-07-01** — Laptop DX (#101): the image now builds **all three** binaries (`/brainiac` http,
  `/kb`, `/brainiac-mcp`), so first use needs **no Go and no exposed ports** — run `docker compose exec
  app /kb …` (the container already has DATABASE_URL + OLLAMA_URL) and point Claude Desktop at
  `docker compose exec -T app /brainiac-mcp`. `./data:/data:ro` mount for `kb import --source markdown`;
  `docker-compose.dev.yml` optionally exposes db/ollama for host tooling. Guide: `docs/laptop.md`;
  Makefile `import`/`kb`/`mcp-config` helpers. (#101)
- **2026-07-01** — Content-defined chunking (#99): replaced positional chunking with a Gear/FastCDC-style
  rolling-hash chunker (`internal/chunk`, deterministic gear table, min/target/max bounds, cuts snapped to
  the nearest line/word break, UTF-8 safe). Boundaries are now **content-defined and self-healing**, so an
  edit near the top of a document only re-embeds the chunk(s) it touches — downstream boundaries
  re-synchronize and their content hashes stay identical (skipped on reconcile), instead of the whole tail
  cascading. Dropped the old size knob and overlap. Unit test proves an early insert changes ≤3 chunks;
  DB test proves re-ingest re-embeds only the local region. (#99)
- **2026-07-01** — Ops & config hardening (#80/#85/#86 — **M5 complete**): compose gains per-service
  `mem_limit`/`cpus` (sized to the 4 GB box) and json-file **log rotation** (via a shared anchor), plus a
  **backup sidecar** (`--profile backup`, daily `pg_dump`, keeps 14). `config.Validate` now also requires
  `embedding.provider/model/base_url`; `config.RedactedDSN` masks the password (used in the startup log).
  `restore.sh --force` runs unattended. (#80, #85, #86)
- **2026-07-01** — Ingestion quality (#81/#83): `chunkText` now splits oversized paragraphs on
  **word/rune boundaries** (never mid-word or mid-rune) with a ~12% **overlap** (word-aligned) so
  boundary-spanning facts stay retrievable. Density selector: `hasEntityLike` catches acronyms/identifiers
  (`API`, `S3`, CamelCase) at the **first word** too; stop-words are **pluggable** via `WithStopwords`
  (the seam for non-English corpora). (#81, #83)
- **2026-07-01** — Startup + connector resilience (#78/#79): `store.ConnectWithRetry` (exponential
  backoff, 60s cap) so `cmd/http`/`cmd/mcp` wait for Postgres instead of crash-looping. The Notion `do`
  now retries **429** honoring `Retry-After` (bounded). (#78, #79)
- **2026-07-01** — Retrieval/robustness bundle (#73/#74/#82/#84): recall traversal is **bounded** —
  `EdgesForNode` takes a limit (50/node) + caps on total edges (100) and evidence (30), so a hub node
  can't flood the bundle. **Embedding-dim validation**: `config.Validate` requires `dims == 768`
  (`model.SchemaEmbeddingDims`) and `Ingest` rejects a wrong-length vector with a clear error (no opaque
  pgvector failure). **Empty queries** short-circuit in `Search`/`Recall` (covers the MCP path). Evidence
  join now filters to `tier='hot'` (cold chunks stay out of answers). (#73, #74, #82, #84)
- **2026-07-01** — Observability + HTTP hardening (#75/#77/#87): request logging (`RequestID`+`Logger`);
  a hand-rolled `internal/metrics` (latency histogram + pull gauges) exposed at **`/metrics`** (Prometheus
  text, no heavy dep) with a `brainiac_vector_index_bytes` gauge (the ★ index-vs-RAM signal via
  `pg_relation_size`); `/api/health` now includes `version`, `vector_index_bytes`, `latency_p50/p95_ms`;
  `/healthz` includes `version`. Embedder outage → **503** (`core.ErrEmbed`) instead of 500; 5xx errors are
  logged server-side and returned generically (no internal leak). Server `Read/Write/Idle` timeouts set.
  Resolves the "★ metrics declared but unmeasured" gap. (#75, #77, #87)
- **2026-07-01** — Secure by default (#69, last P0): the app binds **host-localhost only**
  (`127.0.0.1:8080` in compose) — not the LAN. **Write endpoints are off by default**: mounted only when
  `clients.webui=interactive` AND `AUTH_TOKEN` is set, then gated by `Authorization: Bearer` (constant-time
  compare); merge body capped at 64 KiB. Reads stay open (protect via the Caddy proxy). `server.New` takes
  an `Options{Writable, AuthToken}`. DB-gated test: `/api/merge` is 401 without the token, 200 with. (#69)
- **2026-07-01** — Retrieval relevance floor (#70): `Search` and `Recall` now drop hits beyond a cosine
  cutoff (`MaxRelevantDistance`, default 0.75) so off-topic queries no longer return confidently-cited
  garbage chunks/nodes. Tunable against the eval harness (#29). (#70)
- **2026-07-01** — Edge uniqueness (#71): migration 0003 adds a partial unique index on current edges
  `(from_id,to_id,type) WHERE status='current'` (dedups any pre-existing first). `InsertEdge` is now an
  **upsert** — a repeated `link` refreshes the rationale/provenance instead of creating a duplicate.
  `RepointEdges` (merge) is conflict-safe: colliding edges are marked historical rather than duplicated.
  DB-gated test: re-linking returns the same edge id and refreshes `why`. (#71)
- **2026-07-01** — M5 (production readiness) started. **Actualization + ingest resilience (#68/#72):**
  `Ingest` now reconciles **per document in one transaction** — unchanged chunks kept (by
  `(hash)` for the source), edited-away/removed chunks **deleted** (`DeleteChunksBySourceURINotIn`), new
  chunks inserted; so re-importing an edited doc no longer accumulates stale chunks. `source_modified_at`
  is populated from the connector (Notion `last_edited_time`, Markdown mtime → `RawDoc.ModifiedAt`).
  Embeddings computed outside the tx; the Ollama embedder retries transient failures with backoff
  (`WithRetries`, default 3); a failed document is counted (`IngestStats.Failed`/`Deleted`) and skipped so
  the run continues. Post-roadmap audit recorded in [docs/production-readiness.md](docs/production-readiness.md)
  (#68–#87). (#68, #72) One line per notable decision; link to the PR/issue.

- **2026-07-01** — Markdown connector + seams frozen (#31, **M4 & roadmap complete**):
  `internal/plugins/markdown` implements `plugins.SourceConnector` over a folder of `.md`/`.markdown`
  files (`markdown://<rel>` provenance). `kb import --source markdown --path <dir>`. Built as the
  deliberate **second** connector — the `SourceConnector` interface fit both Notion and Markdown with no
  changes, so the plugin seams are now declared **stable** (PRD §2.3, §20.4). Unit-tested against a temp
  dir. (#31)
- **2026-07-01** — Storage optimizations (#30): `Core.Reembed` rebuilds every chunk's vector from stored
  raw text (the embedding-model-upgrade path, no source re-read — §13.5), exposed as `kb reembed`.
  `Core.SetChunkTier` moves chunks hot↔cold (cold excluded from default search — §13.4). Quantization
  progression (halfvec→int8→binary) is documented in [ADR 0003](docs/decisions/0003-cold-tier-at-scale.md);
  schema is already `halfvec`. DB-gated tests (reembed lowers distance; cold hidden from search). (#30)
- **2026-07-01** — Eval harness (#29): `Core.Eval(golden, k)` runs a golden query set through `search`
  and reports **recall@k** + mean source recall + per-query hits (PRD §18). `kb eval --golden <file> --k`
  prints it; `eval/golden.example.json` shipped. Objective quality proof across growth/model/threshold
  changes. DB-gated test. (#29)
- **2026-07-01** — Backups (#28): `scripts/backup.sh` (gzipped `pg_dump --clean` through the `db`
  container, retention `BACKUP_RETENTION`=14) + `scripts/restore.sh` (confirmed restore) +
  `docs/operations.md` runbook (daily cron). One DB → one consistent snapshot covers graph + vectors +
  provenance (§16). (#28)
- **2026-07-01** — **Test isolation fix.** DB-backed tests across packages share one Postgres and each
  `TRUNCATE` — `go test ./...` runs packages in parallel, so they stomped each other (intermittent
  failures like "recall nodes missing"). CI now runs `go test -race -count=1 -p 1 ./...` (packages
  serialized). Lesson: shared-DB tests must not run concurrently.
- **2026-07-01** — M4 started. Reverse proxy + auth (#27): `Caddyfile` + a `caddy` compose service
  (profile `proxy`) front the app's HTTP (WebUI + REST) with TLS + basic auth; MCP is stdio-only and
  never exposed. Config via `SITE_ADDRESS`/`BASIC_AUTH_USER`/`BASIC_AUTH_HASH`. `docs/deployment.md`
  documents prod setup (drop the app's host port so only Caddy is reachable). CI `caddy` job validates
  the Caddyfile. Cold-tier escalation resolved via [ADR 0003](docs/decisions/0003-cold-tier-at-scale.md) (#34). (#27)
- **2026-07-01** — Graph visualization (#26, **M3 complete**): `GET /api/graph?limit` (`store.GraphSnapshot`
  → `Core.Graph`, edges filtered to returned nodes) + a WebUI Graph tab. Chose a **compact built-in
  force-directed SVG renderer** (~60 lines) over a heavy lib/CDN (Cytoscape) to keep the WebUI a
  single embedded page with **no build step and no external runtime dependency** (self-hosted/offline
  ethos); a richer lib can replace it later for large graphs. DB-gated `/api/graph` test. (#26)
- **2026-07-01** — Interactive WebUI consolidation queue (#25): REST gains write endpoints
  `GET /api/consolidate`, `POST /api/merge`, `POST /api/edges/{id}/confirm|flag-stale`; the WebUI adds a
  Consolidate tab with per-group **Merge** buttons and per-edge **Confirm** buttons (batch review — the
  highest-value screen). All actions call core; nothing bypasses it. DB-gated httptest covers
  consolidate→merge. (#25)
- **2026-07-01** — CLI consolidate (#35): `kb consolidate` prints the librarian report (merge groups /
  conflicts / stale / rollups); `kb merge --keep --drop` applies an approved merge. Scheduling is external
  cron (`0 3 * * 0 kb consolidate`) per config `consolidation.schedule` — documented in §8. (#35)
- **2026-07-01** — M3 started. Consolidation core (#23/#24): migration 0002 adds `edges.flagged_stale`.
  Lifecycle ops `FlagStale`/`Confirm`/`ProposeMerges`; `Consolidate()` runs the librarian pass returning
  merge groups (normalized-name dups), conflicts (same from+type, different target), stale-flagged edges,
  and rollup candidates (≥5 edges) — all human-reviewable, nothing auto-applied. `ApplyMerge` folds a
  duplicate into a keeper (repoint edges, merge aliases, mark historical — reversible), atomic. Health
  gains `% edges stale`. DB-gated test covers the whole flow. (#23,#24)
- **2026-07-01** — Notion connector (#19, ADR 0002 — **M2 complete**): `internal/plugins/notion`
  implements `plugins.SourceConnector` over the Notion API — `Fetch` paginates `/v1/search`, recurses
  page blocks (bounded), flattens rich-text → `RawDoc` (URL provenance + `page_id`/`last_edited_time`
  locator); `Watch` emits upserts. Token via config `sources[].token` / `NOTION_TOKEN`. `kb import
  --source notion` now runs the real ingest. Unit-tested against a mocked Notion API (pagination +
  flattening). Live token/scope to verify at deploy time. (#19)
- **2026-07-01** — Read-only WebUI (#21, M2 done): `internal/webui` embeds a single static page
  (`embed.FS`) with Search / Recall / Health tabs (vanilla JS → `/api/*`); mounted by `server` as a
  catch-all after the API/health routes. UI is a client only — all logic stays in core. Unit tests cover
  embed serving + server mount. (#21)
- **2026-07-01** — REST API (#20, ADR 0001): `internal/server` now uses **chi** and mounts read-only
  `/api/health`, `/api/search?q&k`, `/api/recall?q` over core (alongside `/healthz`/`/readyz`); `cmd/http`
  builds the core and passes it in. Writes stay on MCP/CLI (WebUI is read-only). Added `json` tags to
  `model` (embeddings are `json:"-"`, never sent). DB-gated httptest covers search/health + missing-param
  400. Added chi. (#20)
- **2026-07-01** — Health metrics (#22): `Core.Health()` + `store.HealthCounts` (one round-trip) report
  chunks (hot/cold), nodes (current/historical), edges (current/historical), edges-per-node, and %
  historical (§14). `kb health` now prints them. REST `/health` exposes them in #20. Stale/conflict
  metrics come with M3 consolidation. DB-gated test. (#22)
- **2026-07-01** — Ingest pipeline (#18): `Core.Ingest(connector, opts)` runs fetch → chunk → select →
  embed → store per-chunk (PRD §8). Content-hash dedup skips unchanged chunks; Drop is discarded, Keep
  stored hot, Queue stored cold (out of default search). `Core` now also holds a `plugins.Selector`
  (interface — wired by clients as `density.New()`); `store.ChunkExistsByHash` added. DB-gated test
  covers selection + idempotent re-ingest. (#18)
- **2026-07-01** — M2 started. Density selector (#17): `internal/plugins/density` implements
  `plugins.Selector` — structural filter (empty/near-empty/short) + a no-LLM density heuristic
  (content-word ratio, lexical diversity, entity-like + number signals) → keep/queue/drop with reversible
  thresholds. Unit-tested. Wired into ingest next (#18). Transport + Notion paths resolved via ADRs
  ([0001](docs/decisions/0001-core-webui-transport.md), [0002](docs/decisions/0002-notion-ingestion-path.md); #32/#33).
- **2026-07-01** — cobra CLI (#16, M1 complete): `kb` exposes `migrate`, `health`, `search`, `recall`,
  `remember`, `link`, `supersede` over core, plus `import/refresh/consolidate/reembed` stubs that error
  with their issue ref. Command tree unit-tested; errcheck configured to ignore `fmt.Fprint*` (CLI output
  errors aren't actionable). **M1 done — capture→recall works from Claude (MCP) and the CLI.**
- **2026-07-01** — **Gitignore gotcha fixed.** An unanchored `ollama/` rule in `.gitignore` shadowed the
  `internal/plugins/ollama` source package, so the embedder (#8, PR #46) merged with **no source files** —
  CI stayed green only because nothing imported it yet. Caught when `cmd/mcp` imported it and CI failed
  ("no required module provides package"). Fix: root-anchor volume rules (`/ollama/`, `/data/`, `/pgdata/`,
  `/build/`, `/dist/`) and add the package. Lesson: anchor volume/dir ignore rules with a leading `/`.
- **2026-07-01** — MCP server (#15): `internal/mcpserver` exposes search/remember/link/recall/supersede
  as typed MCP tools (official Go MCP SDK v1.6.1, `AddTool[In,Out]` with auto JSON-schema) forwarding to
  core — no logic in the client. `cmd/mcp` wires config→pool→migrate→Ollama embedder→core→stdio (logs to
  stderr to keep the protocol stream clean). DB-gated round-trip test uses the SDK's in-memory transport
  (real client→MCP→core: remember→link→recall). **Claude can now capture and recall via MCP.** (#15)
- **2026-07-01** — Core ops recall + supersede (#13/#14): `Recall` composes vector search + node
  proximity + edge traversal (incl. supersedes history) + join of raw chunks by `source_uri` into a
  cited evidence bundle (`RecallResult`; §10). `Supersede` adds a `supersedes` edge (new→old) and marks
  the old node historical, atomically (replacement not deletion, §11.2). Added store graph helpers
  (`GetNodeByID`, `EdgesForNode`, `GetChunksBySourceURI`, `UpdateNodeStatus`, shared `scanEdge`).
  **The capture→recall loop now works end-to-end** (DB-gated tests in CI). (#13,#14)
- **2026-07-01** — Core ops search/remember/link (#10/#11/#12): `Core` now holds `pool + embedder`.
  `Search` embeds the query → cosine kNN. `Remember` upserts a node — exact-name is idempotent (aliases
  merged); otherwise inserts and **flags** duplicate candidates by normalized name (strip non-alnum,
  "Order Service"=="OrderService") and summary-embedding proximity (≤0.15 cosine), never auto-merging
  (§11.1). `Link` creates the edge and any missing endpoint nodes in **one transaction** (capture flow,
  §9). Added store node finders (`FindNodesByNormalizedName`, `FindSimilarNodes`, `UpdateNodeAliases`).
  DB-gated core test (CI) covers create/idempotent/dedup/link/search. (#10,#11,#12)
- **2026-07-01** — Ollama embedder (#8): `internal/plugins/ollama` implements `plugins.Embedder` over
  `POST /api/embeddings` (`{model,prompt}` → `{embedding}`), []float64→[]float32, non-2xx/empty = error
  (caller queues on failure, §11). Injectable HTTP client; unit-tested via `httptest` (no Ollama needed).
- **2026-07-01** — Data-access layer (#9, PR mislabeled as #8): domain types in `internal/model` (Chunk/Node/Edge); repository
  functions in `internal/store` (InsertChunk, SearchChunks by cosine, InsertNode, GetNodeByCanonicalName,
  InsertEdge, ListEdgesFrom) taking a `DBTX` (pool or tx), plus `WithTx` for atomic writes. Embeddings
  sent as text + `::halfvec` cast (no type registration — avoids the fresh-DB boot chicken-egg). Numerics
  cast to `float8` on read to keep pgx scans simple. DB-gated test (CI) covers insert/search/traverse/
  rollback; added pgvector-go. (#8)
- **2026-07-01** — M1 started. The four plugin interfaces + shared value types (`RawDoc`, `Change`,
  `Entity`/`Relation`/`Extraction`, `Score`/`Decision`) landed in `internal/plugins`, with a generic
  `Registry[T]` for config-by-name selection. Connectors use Go 1.23 range-over-func iterators
  (`iter.Seq2`). No implementations yet (one per seam later). Fully unit-tested. (#7)
- **2026-07-01** — One-command Docker Compose deploy landed (M0 done): `Dockerfile` (multi-stage →
  distroless static, ~small image), `docker-compose.yml` (db + ollama + ollama-pull + app), `.env.example`.
  App auto-migrates on boot and exposes `/healthz` + `/readyz` (`internal/server`); container
  `HEALTHCHECK` uses `/brainiac healthcheck`. A CI `smoke` job boots db+app and asserts `/readyz` db:ok —
  self-verifying deploy without local Docker. Readiness gates on DB only; Ollama optional (§11). (#3)
- **2026-07-01** — Config system (`internal/config`): single YAML (PRD §19) + env overrides
  (`DATABASE_URL`/`OLLAMA_URL`/`HTTP_ADDR` win over the file), `Default()` + `Validate()`; `config.yaml`
  path via `BRAINIAC_CONFIG`. `config.example.yaml` shipped; `kb migrate` now reads config. Fully
  unit-tested locally (no DB). yaml.v3 added. (#5)
- **2026-07-01** — Core schema (`chunks`/`nodes`/`edges`, halfvec(768), HNSW on hot chunks) + a tiny
  embedded forward-only migration runner (`internal/store`) landed; `kb migrate` wired; validated in CI
  against the pgvector service (local `go test` skips without `DATABASE_URL`). Chose a ~60-LOC runner
  over goose — no external migration dep, forward-only matches our stable-schema stance. Added pgx as
  the only DB dependency. (#4)
- **2026-07-01** — Go module scaffolding landed: `internal/core` (sole logic home) + `internal/plugins`,
  thin clients `cmd/cli` (binary `kb`), `cmd/http` (`brainiac-http`), `cmd/mcp` (`brainiac-mcp`),
  zero external deps yet. `Makefile` (fmt/lint/test/build/up/down), golangci-lint v2, version via
  `-ldflags`. Binaries ~2.4 MB. (#2)
- **2026-07-01** — **Language set to Go** (was tentatively Python). The app has no in-process ML
  (embeddings are Ollama-over-HTTP), so Python's ecosystem edge does not apply; Go wins on the two hard
  requirements — single static binary / tiny image (deploy) and low RAM on the 4 GB box — and matches
  goroutly. Stack: net/http+chi, pgx+pgvector-go, goose, cobra, Go MCP SDK. §3 updated. (#37)
- **2026-07-01** — Bootstrapped repo + full milestone backlog (#1–#35). Postgres/pgvector + Ollama +
  Docker Compose. Rationale captured in §3. (#1)

---

## 11. Failure modes & graceful degradation

| Failure | Effect | Mitigation |
|---|---|---|
| Ollama down | No new embeddings; existing search works | Queue ingest; graph capture unaffected |
| Index spills to disk | Slow search | Quantize / add RAM / tier (§7, §9) |
| Bad merge in dedup | Two real entities collapsed | Merges human-approved + reversible (alias history kept) |
| Stale knowledge served | Wrong "why" | Staleness flags + provenance let the reader verify vs source |
| Graph fragments (no dedup) | Disconnected islands, weak recall | Librarian pass is mandatory |
| Logic duplicated in a client | Claude vs WebUI disagree | All logic in core; clients call core only |
| 4 GB OOM on large corpus | Crash | Prototype tier only; size up before real load |

---

## 12. Open questions

- ~~Notion ingestion path~~ — **resolved**: native API connector, see [ADR 0002](docs/decisions/0002-notion-ingestion-path.md) (#32).
- ~~Core↔WebUI transport~~ — **resolved**: REST (net/http+chi), MCP separate, see [ADR 0001](docs/decisions/0001-core-webui-transport.md) (#33).
- ~~Cold-tier tech if the archive outgrows pgvector~~ — **resolved**: escalation ladder (selection →
  quantization → Matryoshka → tiering → external cold store), see [ADR 0003](docs/decisions/0003-cold-tier-at-scale.md) (#34).
- Whether to ever introduce a local consolidation LLM, or keep all LLM work in Claude-in-chat. **Partly
  resolved (#164):** the *extraction* half is now a local-optional Ollama chat model (opt-in, review-gated,
  §7/§8); consolidation itself still runs in Claude-in-chat.
- **Multi-project / multi-team memory** (#113) — reframed as two independent axes:
  - **Identity** (should same-named entities merge) — **resolved & partly shipped**: identity = `canonical_name` +
    a declared **discriminator** set (`project`, `env`, …; empty = global), so same-named entities in different
    projects stay distinct without any wall (#117 shipped; the agent passes its `project` per call as the
    discriminator, #116 shipped; Consolidate scoped to identity, #118 shipped). Any axis is expressible
    (`project`/`env`/`client`, #125 shipped); the intended UX is **reactive** — introduce an axis only when a
    real conflation appears (#126 `disambiguate` op shipped; #127 tangled-node split + librarian detector,
    shipped), not a declared-upfront vocabulary. Descriptive **facets** are not identity.
  - **Visibility** (should you see across projects) — **soft by default**: one graph, a per-project recall/search
    lens over chunks + nodes that widens on demand (#119 shipped — pass `project` to scope, omit to span all).
    **Hard** isolation (read-scope + security) is now an opt-in Layer 2 for privacy/compliance/multi-tenant —
    **foundation shipped** (#185, part of #120): one server serves many namespaces behind a **per-token
    principal** wall, enforced in the core. Follow-ups tracked separately: per-namespace quotas (#186),
    export/backup (#187), whole-namespace delete + handoff (#188), WebUI read-auth (#189). Off by default
    (no principals ⇒ Layer 1, unchanged).
