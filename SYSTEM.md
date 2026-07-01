# Brainiac — System Specification

> **This is the living spec.** Read it before working on any task. Update it in the *same* PR whenever
> you add, change, or remove a feature, or discover a constraint/edge case. Every "why" that matters
> lives here — code says *what*, SYSTEM.md says *why it is this way*.

**Status:** M0 + M1 complete. One-command deploy, plugin seams, Ollama embedder, data-access, the full
core operation set (search/remember/link/recall/supersede), the MCP server, and the cobra CLI all landed
— the **capture→recall loop works end-to-end from both Claude (MCP) and the CLI**. Next: M2 (ingestion,
selection, Notion connector, REST API, read-only WebUI). See the backlog on GitHub.
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
  source_locator jsonb, quality_score, tier(hot|cold), content_hash, created_at, source_modified_at`.
  HNSW cosine index on `embedding WHERE tier='hot'`.
  - *Raw text is mandatory:* needed to answer, and to **re-embed on model change without re-reading
    sources** (§7 optimization).
- **`nodes` (Layer 2)** — `id, canonical_name, aliases[], type, summary_embedding halfvec(768),
  status(current|historical), created_at, last_confirmed_at`. `summary_embedding` powers semantic dedup.
- **`edges` (Layer 2)** — `id, from_id, to_id, type, why, source_uri, source_locator, author, status,
  created_at, last_confirmed_at`. FK indexes on `from_id`/`to_id`.

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

**Retrieval flow (`recall`):** vector search → graph lookup for the entity → traverse edges → join raw
chunks by `source_uri` → Claude synthesizes **with citations**. Every claim maps to `source_uri` +
locator. **An answer without a source is a quality bug.**

**Capture flow (default, chat-driven):** human investigates → tells Claude to save it → Claude calls
`remember`/`link` → core upserts node(s) + edge (with `why`, provenance, author) in one transaction. No
pipeline LLM; the "extraction" is the chat itself.

---

## 7. Plugin seams + ingestion + storage optimizations

**Four seams** (interfaces from the start, one impl each for v1):
- `SourceConnector.fetch()/watch()` — "give me documents, tell me when they change." v1: Notion.
- `Extractor.extract(chunk)` — text → nodes/edges. v1 default: **chat-driven** (bypassed; Claude
  supplies the structure). Optional `local-llm` (Ollama + structured output) for bulk.
- `Selector.score(chunk)` — the water filter; keep/queue/drop. v1: `density-filter`.
- `Embedder.embed()/dims()` — v1: Ollama nomic-embed-text.

**Ingestion pipeline (selection *before* the index — PRD §8):** structural filter (free rules) → density
heuristic (unique nouns/terms, entities/numbers) → chunk then select **per-chunk** → LLM gatekeeper on
the borderline queue only (Ollama small model *or* deferred Claude batch) → embed + store raw text +
provenance + `quality_score`. Thresholds are **reversible** because raw text + score are stored.

> **Day-one:** the ingest script is *not* required. Prototype ingest goes **through Claude** (paste a
> link/export; Claude reads → selects → calls `remember`/`link`). Write connector automation only when
> ingest becomes a repeatable bulk/scheduled routine.

**Storage optimizations (apply progressively, by need — PRD §13):** selection (strongest lever) →
quantization (`halfvec` → int8 → binary + re-rank) → Matryoshka dim reduction (768→256) → hot/cold
tiering → **re-embed from stored raw text** on model upgrade (why raw text is mandatory).

---

## 8. Consolidation ("Librarian" pass)

Scheduled or on-demand; walks the graph (small), not the corpus. Drivable by Claude-in-chat or a local
Ollama LLM; reviewable in the WebUI consolidation queue.

1. **Node dedup / canonicalization** — propose merges by name similarity or `summary_embedding`
   proximity; **human confirms** (auto-merge collapses real entities — always reversible, alias history
   kept). Without this the graph fragments into disconnected islands.
2. **Replacement, not deletion** — `supersedes` edge + `status=historical`.
3. **Staleness** — if `source_modified_at > edge.created_at`, flag "possibly stale, verify."
4. **Conflict detection** — surface contradictions for human resolution.
5. **Rollups** — a node with many edges gets a "current state of X" summary linking to detailed history;
   creates the two reading levels (*what is now* over *how we got here*).

The librarian pass is **mandatory, not optional** — skipping node dedup is the top failure mode (§11).

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

**Evaluation:** a golden query set (~20–50 questions with expected sources) run at every notable growth
step and after model/threshold changes; citation discipline (uncited answer = quality bug); capture rate
as the adoption signal.

---

## 10. Decision Log

Newest first. One line per notable decision; link to the PR/issue.

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
- Cold-tier tech if the archive outgrows pgvector (Qdrant/Milvus) and the two-store join at that scale (#34, M4).
- Whether to ever introduce a local consolidation LLM, or keep all LLM work in Claude-in-chat.
- Multi-team isolation vs shared graph (namespaces vs one corpus).
