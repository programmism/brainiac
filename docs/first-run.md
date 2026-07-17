# First run — your first decision in 10 minutes

This is the shortest path from *nothing* to **"I put one decision in and recalled
the *why* back out."** It uses only the CLI wrapper — no agent, no cloud, no GPU.
When you're done, [wire it to Claude over MCP](#5-connect-your-agent-mcp) so your
agent does this automatically.

> New to the moving parts? Skim [concepts-and-workflows.md](concepts-and-workflows.md)
> first. Running on a laptop? [laptop.md](laptop.md) has the `./brainiac` wrapper
> details.

---

## 1. Start it (2 min + a one-time model download)

```bash
git clone https://github.com/programmism/brainiac && cd brainiac
cp .env.example .env
./brainiac up
```

The very first boot downloads the local embedding model (~270 MB). Watch it:

```bash
./brainiac logs ollama-pull      # wait for "success" / the pull to finish, then Ctrl-C
```

Then confirm the stack is actually ready — not just "up":

```bash
curl -s localhost:8080/readyz | jq .      # {"status":"ready", ...}
./brainiac health                          # db ok, embedder ok, counts
```

If `/readyz` isn't green yet, see [Troubleshooting](#troubleshooting) — the usual
cause is the model still downloading. The WebUI is at **http://localhost:8080**.

> 📸 *(screenshot to capture on a live deploy: the WebUI **Health** tab showing db
> ok / embedder ok — save as `docs/images/first-run-health.png`.)*

---

## 2. Put one decision in (2 min)

A decision is two entities and a **relationship that carries a `why`**. Let's
record: *OrderService writes to Kafka — for durability during peak load.*

```bash
# Two entities (nodes). --summary is embedded for semantic search + dedup.
./brainiac remember "OrderService" --type service \
  --summary "Handles order placement and fan-out."
./brainiac remember "Kafka" --type datastore \
  --summary "Durable append-only event log."

# The relationship (edge) — the why + who decided is the whole point.
./brainiac link --from "OrderService" --type "writes-to" --to "Kafka" \
  --why "durability during peak load — DB writes dropped events at 1200 orders/min" \
  --author alice
```

Each `remember` prints `created … (id=…)`; `link` prints `linked OrderService
-writes-to-> Kafka (edge=…)`. If `remember` flags a **duplicate?**, that's the
semantic dedup working — a near-identical entity already exists.

---

## 3. Recall the *why* back out (1 min)

This is the payoff — ask in natural language, get the decision *and* its rationale:

```bash
./brainiac recall "why does OrderService use Kafka"
```

You'll see the node summaries, the **edge with its `why`**, and any supporting
chunks. Look up the entity directly to see its full record + edges:

```bash
./brainiac node "OrderService"
```

> 📸 *(screenshot to capture: the WebUI **Recall** tab for "why does OrderService
> use Kafka", showing the edge `why` — save as `docs/images/first-run-recall.png`.
> And the **Graph** tab showing the OrderService → Kafka edge —
> `docs/images/first-run-graph.png`.)*

---

## 4. Bulk-add from your docs (optional, 1 min)

Drop Markdown/txt/HTML/DOCX files into **`./data/docs/`** and they're chunked,
embedded, and searchable — automatically within a minute (`INGEST_INTERVAL=60s`),
or immediately with:

```bash
./brainiac import
./brainiac search "your question"     # semantic search over the chunks
```

To also derive graph nodes/edges from those docs automatically, enable a
server-side extractor — see [SYSTEM.md §7](../SYSTEM.md) (`EXTRACTOR=local-llm`
for a self-hosted model, or `EXTRACTOR=claude` + `ANTHROPIC_API_KEY`).

---

## 5. Connect your agent (MCP)

So Claude recalls before it answers and saves what you learn — automatically.
The config path is generated for you (no hand-editing):

```bash
./brainiac mcp-config          # prints a ready-to-paste Claude Desktop config
```

Paste the output into Claude Desktop's `claude_desktop_config.json`
(**Settings → Developer → Edit Config**), then **fully quit and reopen Claude
Desktop**.

**Verify it's wired** — in a new Claude chat, ask:

> *"Using brainiac, recall why OrderService uses Kafka."*

Claude should call the `recall` tool and answer with the `why` you stored in
step 2. If it doesn't see the tool, the stack isn't running or the config didn't
load — quit Claude fully (not just close the window) and confirm `./brainiac
health` is green.

> 📸 *(screenshot to capture: Claude Desktop showing the brainiac MCP tools
> connected — `docs/images/first-run-mcp.png`.)*

---

## Troubleshooting

| Symptom | Cause & fix |
|---|---|
| `/readyz` returns 503, or search/recall 503s | **The embedding model is still downloading** on first boot. Watch `./brainiac logs ollama-pull`; it's a one-time ~270 MB pull. `/readyz` goes green once it finishes. |
| Boot log: `embedding.dims must be N to match the schema, got M` | **Dimension mismatch** — the embedder's output size must equal the schema's. Don't change `embedding.dims`/`model` away from the shipped defaults (`nomic-embed-text`, 768) unless you also migrate the vector column. Revert `.env`/`config.yaml` to the defaults and `./brainiac up` again. |
| `embedder unreachable` warning in the startup banner | Ollama isn't up yet or `OLLAMA_URL` is wrong. Give it a few seconds after `./brainiac up`; check `./brainiac logs ollama`. |
| `./brainiac up` fails / port 8080 busy | Another process holds `:8080`. Stop it, or set `HTTP_ADDR` in `.env`. |
| Claude doesn't see the brainiac tools | The stack must be running (`./brainiac health` green) **and** Claude Desktop must be **fully quit and reopened** after editing the config. Re-run `./brainiac mcp-config` to confirm the path matches your checkout. |
| Writes rejected / WebUI buttons do nothing | Writes are off by default (secure). Set `WEBUI_MODE=interactive` **and** `AUTH_TOKEN` in `.env` (generate one with `./brainiac token gen`), restart, and paste the token into the WebUI. |

More: [operations.md](operations.md) (backups, updates), [deployment.md](deployment.md)
(proxy/TLS), [security.md](security.md) (hardening).
