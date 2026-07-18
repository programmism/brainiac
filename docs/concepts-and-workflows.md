# Concepts & Workflows

A task-oriented guide for **operators** — people curating the memory from the CLI (`kb`) or the WebUI
consolidation queue. If you drive Brainiac only through Claude in chat, the MCP tools guide themselves
(see [agent-memory.md](agent-memory.md)); this guide is for when you sit *behind* the memory and keep it
healthy.

> This is a *how/when* guide. For *why the system is built this way*, see [SYSTEM.md](../SYSTEM.md).

> **One word for scoping: "project".** As a user you only need **project** — the
> `--project` / `project` you pass to scope a memory to a team or repo (empty =
> global/shared). The other words are internals you'll rarely touch:
> *namespace* = a project as an isolation boundary; *scope* = the provenance label
> on a recall result (`global` / `project:X`); *discriminator* = the identity axis
> a project is one value of (advanced: `env`, `client`, …); *principal* = a bearer
> token bound to namespaces under hard isolation ([security.md](security.md)). If
> in doubt, think "project".

---

## The mental model: two layers

Brainiac stores knowledge in **two layers**, and most confusion comes from mixing them up.

| | **Layer 1 — chunks (semantic search)** | **Layer 2 — graph (curated)** |
|---|---|---|
| **What** | Raw text split into chunks, embedded as vectors | Entities (nodes) and relationships (edges) |
| **Grows** | Automatically, with the corpus (import/ingest) | Deliberately, with human/Claude effort |
| **Answers** | *"What do we have about X?"* (`search`) | *"Why / how is X the way it is?"* (`recall`) |
| **Carries** | Provenance + a quality score | A `why`, provenance, author — a memory of **decisions** |
| **Truth model** | Evidence — two sources may disagree; the reader judges | Curated — contradictions are surfaced and resolved |

**Rule of thumb:** dumping documents fills Layer 1. Capturing *"A writes to B; we rejected sync because
of peak load"* builds Layer 2. Retrieval uses both — `recall` returns graph rationale **plus** the chunks
behind it. Layer 2 is where a fact can go stale, be superseded, or conflict; Layer 1 is just breadth.

---

## The verbs — what each does and when to use it

Reach for the **everyday verbs** by default (`capture`, `search`, `recall`); the rest are finer-grained
controls you only need when the everyday path isn't enough. See "Everyday vs advanced" below.

| Verb | Layer | What it does | Reach for it when… |
|---|---|---|---|
| **capture** | 2 | One-shot: creates both entities (with optional one-line summaries) and links them with `why` in a single call | The everyday "record this decision/fact" — the simplest way to write memory |
| **search** | 1 | Vector search over chunks, cited | You want relevant passages on a topic |
| **recall** | 1+2 | Vector + graph traversal + evidence bundle | You want the *why/how*, with rationale and citations |
| **remember** | 2 | Upsert an entity (node); returns dup candidates, never auto-merges | Saving a new entity (service, decision, person, …) |
| **link** | 2 | Record a relationship (edge) with `why` + provenance; missing endpoints are created | Capturing *A —relates_to→ B* and the reason |
| **add_document** | 1 | Store a text you already have, chunked + embedded; re-adding a `source_uri` updates it | Claude read a page elsewhere and wants it searchable |
| **import / ingest** | 1 | Bulk-import a Notion page or a markdown dir | Pulling a source in wholesale |
| **supersede** | 2 | New **node** replaces old: adds a `supersedes` edge, old → `historical` (kept, not deleted) | A decision changed — keep the "why we changed our minds" |
| **disambiguate** | 2 | Re-scope one node by adding identity axes (`env=prod`) | You realize a node conflates two things; move the whole node under an axis |
| **consolidate** | 2 | The "librarian pass" — proposes merges/splits/conflicts/stale/rollups. **Proposes only** | Periodically, to review graph health (the review queue) |
| **merge** | 2 | Fold a duplicate node into a keeper (edges repointed, aliases kept, reversible) | Two nodes are the *same* entity |
| **split** | 2 | Carve a tangled node into scoped children, routing its edges | One node's edges contradict — it's really two entities (prod vs staging) |
| **retire-edge** | 2 | Mark one **edge** `historical` (edge-level supersession) | Two edges conflict; keep the correct one, retire the loser |
| **confirm** / **flag-stale** | 2 | Clear / set an edge's "verify me" flag | An edge was flagged stale and you've checked it (or want to flag it) |
| **proposals** | 2 | List pending nodes/edges the optional local-LLM extractor suggested (empty unless it's enabled) | Reviewing what bulk extraction proposed |
| **review_proposal** | 2 | Approve (→ live) or reject (→ historical) a proposed node/edge | Curating the extractor's output before it enters the memory |

`capture`/`search`/`recall`/`remember`/`link`/`supersede`/`disambiguate` are available over **MCP** (Claude
drives them) and **CLI**. `proposals`/`review_proposal` are on **MCP + WebUI** (the extractor review queue).
`consolidate`/`merge`/`split`/`retire-edge`/`confirm` are **operator** verbs — the review-and-repair
surface (CLI + WebUI), not things Claude does mid-conversation.

### Everyday vs advanced

Most capture is one verb: **`capture`** (record a decision/fact — both entities + the why in one call),
plus **`search`** and **`recall`** to read it back. `remember` and `link` are the finer-grained writes
underneath `capture` — reach for them only when you need to create an entity without a relationship, or
add a link between entities that already exist.

Everything else is **advanced curation** you shouldn't reach for in normal use — the agent (via the
consolidation pass) and operators handle it: `disambiguate` and `supersede` (re-scope / replace an entity),
`rollup` (summarize a busy hub into a "current state of X"), and `as_of` (time-travel reads). They exist for
when curation must be explicit; day-to-day, let `capture` write and consolidation tidy up in the background.

### Type conventions

Node and edge `type` are free text, but **consistency matters**: conflict detection keys on
*same from + same type → different targets*, so a contradiction written with a different type name
(`writes_to` vs `publishes_to`) goes undetected, and the graph fragments. Reuse an existing type over
inventing a synonym.

A non-binding **seed vocabulary** to reach for first:

- **Entity types:** `service`, `datastore`, `decision`, `constraint`, `team`, `person`, `document`, `tool`.
- **Relationship types:** `writes_to`, `reads_from`, `depends_on`, `owns`, `rejected`, `supersedes`,
  `part_of`, `caused_by`.

Case and separators are **normalized on write** (#156): `writes-to`, `writesTo`, `Writes To` all fold into
`writes_to`. That fold is intentionally shallow — it never merges *synonyms*, so `publishes_to` stays
distinct from `writes_to`. When in doubt, `recall`/`search` first and match the type already in use.

---

## The consolidation queue, decoded

`kb consolidate` (or the WebUI **Consolidate** tab) walks the graph — never the whole corpus — and
**proposes** work. Nothing is applied automatically; you decide. Five sections:

### Merge candidates
Nodes that share a normalized name **and** identity scope — likely duplicates (`OrderService` ↔
`Order Service`). **Action:** if they're the same thing, `kb merge --keep <id> --drop <id>` (or the
WebUI button). Reversible; aliases are preserved. Same-named nodes in *different* projects are never
proposed — that's intentional scoping, not a duplicate.

### Conflicts
Two **current** edges from the same node, same relationship type, pointing at **different** targets
(`OrderService —writes_to→ Kafka` **vs** `→ RabbitMQ`). One is probably outdated. **Action:** keep the
correct one; retire the other with `kb retire-edge <id>` (WebUI: **keep « X »**). The retired edge goes
`historical` — still reachable via `recall` history, not deleted.

### Split candidates
One node whose edges contradict (same from+type, ≥2 targets) — a sign it **conflates two entities** that
should be separated by an axis (e.g. one `Config` node that's really prod-Config and staging-Config).
**Action:** `kb split --node <id> --axis env --route <edgeId>=prod --route <edgeId>=staging`. This carves
the node into scoped children and repoints each edge. (Reactive counterpart to `disambiguate`, which
moves a whole node up front.)

### Stale edges
Edges flagged "possibly stale, verify" — either flagged by hand, or auto-flagged because their **source
changed** since the edge was last recorded/confirmed. **Action:** check the source; `kb`/WebUI
**Confirm** if it still holds (refreshes the timestamp, clears the flag), or `retire-edge` if it doesn't.
A confirmed edge won't re-flag until the source changes *again*.

### Rollup candidates
Hub nodes with many edges — candidates for a "current state of X" summary that links to the detailed
history, giving two reading levels (*what is now* over *how we got here*). **Action:** informational for
now; use it to spot nodes worth summarizing.

---

## Scenario → action

| You notice… | Do this |
|---|---|
| Two entities with (nearly) the same name that are the same thing | `consolidate` → `merge --keep --drop` |
| One entity that's really two (prod vs staging, two people same name) | up front: `disambiguate <id> --disc env=prod`; after the fact: `consolidate` → `split` |
| A decision changed (we now use Kafka, not RabbitMQ) | `supersede --old <id> --new <id> --why "…"` (keeps history) |
| Two edges that contradict (same source+type, different target) | `consolidate` → `retire-edge <losing-edge-id>` |
| An imported source was edited after you drew conclusions from it | `consolidate` surfaces it under **Stale** → `Confirm` or `retire-edge` |
| The same name should mean different things per project | save with `--project X` (scopes identity + the retrieval lens) |

---

## Scoping in one paragraph

`--project X` (or the `project` arg over MCP) scopes both **identity** (a node/edge is distinct per
project) and the **retrieval lens** (search/recall see *project + global*). Omit it to work globally /
search across everything. It's a **soft** lens — global results still surface for a scoped query, and
they're marked as such (results carry a `scope` of `global` or `project:NAME`; if a scoped query finds
nothing in-project you'll see a "showing global memory" fallback). Hard per-team isolation is a separate,
future concern.

---

**See also:** [laptop.md](laptop.md) (install + everyday CLI) · [agent-memory.md](agent-memory.md)
(wire it as Claude's memory) · [SYSTEM.md](../SYSTEM.md) (the *why* behind all of this).
