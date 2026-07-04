# Brainiac — System Specification

> **This is the living spec.** Read it before working on any task. Update it in the *same* PR whenever
> you add, change, or remove a feature, or discover a constraint/edge case. Every "why" that matters
> lives here — code says *what*, SYSTEM.md says *why it is this way*.

**Status:** M0–M4 roadmap complete; now hardening for real production use (**M5**, see
[docs/production-readiness.md](docs/production-readiness.md)).

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
- **`nodes` (Layer 2)** — `id, canonical_name, aliases[], type, summary_embedding halfvec(768),
  status(current|historical), discriminators jsonb, scope_key text, created_at, last_confirmed_at`.
  `summary_embedding` powers semantic dedup. **Node identity = `canonical_name` + `discriminators`**
  (the identity-bearing axes: `project`, `env`, …; empty = global/shared). `scope_key` is their canonical
  serialization (sorted `k=v;` pairs, written by the app) and keys idempotent upsert + dedup, so same-named
  entities in different projects stay distinct while universal ones accrue globally (#117, §12).
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

**Retention on source deletion (intentional).** Editing a source updates its chunks (per-source
reconcile); **deleting a source file does NOT remove already-imported content** — Brainiac is a *memory*,
so what it learned persists even after the source disappears. Ingest only reconciles sources it still
sees; it never prunes vanished ones (#107, decided to keep). To drop content: `docker compose down -v`
(wipe all) or delete the specific rows in Postgres.

---

## 8. Consolidation ("Librarian" pass)

Scheduled or on-demand; walks the graph (small), not the corpus. Drivable by Claude-in-chat or a local
Ollama LLM; reviewable in the WebUI consolidation queue.

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
3. **Staleness** — if `source_modified_at > edge.created_at`, flag "possibly stale, verify."
4. **Conflict detection** — surface contradictions for human resolution.
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

**Evaluation:** a golden query set (~20–50 questions with expected sources) run at every notable growth
step and after model/threshold changes; citation discipline (uncited answer = quality bug); capture rate
as the adoption signal.

---

## 10. Decision Log

Newest first.

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
- Whether to ever introduce a local consolidation LLM, or keep all LLM work in Claude-in-chat.
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
    **Hard** isolation (read-scope + security) stays a future, opt-in Layer 2 for privacy/compliance/multi-tenant
    (#120); until then, hard isolation = a separate stack per team.
