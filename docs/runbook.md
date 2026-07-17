# Operational runbook

First-response steps for the Prometheus alerts in
[`deploy/monitoring/brainiac.rules.yml`](../deploy/monitoring/brainiac.rules.yml).
Load the rules and scrape config into your Prometheus (see
[`prometheus-scrape.yml`](../deploy/monitoring/prometheus-scrape.yml)) — and keep
`/metrics` **off the public internet** (it's unauthenticated; scrape it over the
internal network).

Quick triage commands (from the compose checkout):

```bash
./brainiac health                       # db / embedder / counts at a glance
curl -s localhost:8080/readyz | jq .    # readiness (DB + embedder)
docker compose logs --tail=200 app      # recent app + JSON access logs (#258)
docker compose stats --no-stream        # per-container CPU/mem
```

---

## BrainiacDown
Prometheus can't scrape `/metrics` for 2 minutes.
1. `docker compose ps` — is `app` running/healthy? `docker compose logs app`.
2. If it's crash-looping, the usual cause is the DB being unreachable (`connect db`)
   or a failed migration — check `docker compose logs db` and the app's boot lines.
3. Roll back a bad update: `./brainiac update` has healthcheck-based rollback, or
   `git checkout <previous tag> && docker compose up -d --build`.

## BrainiacHighErrorRate
More than 5% of `/api` responses are 5xx over 5 minutes.
1. `docker compose logs --tail=200 app` — the JSON access log shows `status` +
   `path` + `request_id`; grep `"status":5` to find the failing route.
2. Search/recall 5xx is almost always the **embedder**: `./brainiac health` →
   `embedder`. If the model is still pulling, wait (see the first-run
   troubleshooting box); if unreachable, check `docker compose logs ollama`.
3. Other routes: check DB health (`readyz`), and whether a quota/rate limit is
   returning 4xx (those are not 5xx — this alert is server faults only).

## BrainiacSlowSearchP95
`/api/search` p95 latency > 500ms for 10 minutes (target is ~200ms).
1. First check **BrainiacVectorIndexExceedsHalfRAM** — index spilling past RAM is
   the most common cause (SYSTEM.md §9).
2. Check embedder latency (each search embeds the query): `docker compose logs
   ollama`; a GPU helps at scale (`docker-compose.gpu.yml`, #252).
3. Check DB load / autovacuum; a large historical fraction can bloat scans — run a
   consolidation pass.

## BrainiacVectorIndexExceedsHalfRAM
The hot-tier HNSW index no longer fits in ~½ the container RAM, so query p95 rises
as it spills to disk (the ★ scaling ratio, #256).
1. **Raise memory** (`mem_limit` in compose / a bigger box), or
2. **Shrink the hot tier**: tighten ingestion selection so fewer chunks are `hot`,
   or archive cold data. `brainiac_chunks_hot` is the driver.

## BrainiacMemoryNearLimit
The `app` container is above 90% of its `mem_limit` for 10 minutes — OOM-kill risk.
1. Raise `mem_limit` (base compose is sized for a 4 GB box).
2. Correlate with the index-vs-RAM alert; the vector index dominates memory.

## BrainiacHighStaleEdges
Over 20% of current edges are flagged stale (source content changed under them).
1. This is a **curation** signal, not an outage. Review flagged edges in the WebUI
   Consolidate tab or `brainiac` CLI, then confirm/retire them.
2. Persistent high staleness suggests sources churn faster than review — schedule
   consolidation.

## BrainiacHighHistoricalNodes
Over 50% of nodes are historical (superseded). Usually benign (healthy versioning),
but a **consolidation** pass keeps search focused on current state. Not urgent.
