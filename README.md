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

## Quickstart (target)

```bash
git clone https://github.com/programmism/brainiac
cd brainiac
cp .env.example .env      # sane defaults; set your secrets
docker compose up         # → db (pgvector) + ollama + app, migrations + model pulled automatically
```

Easy deployment is a hard requirement: one command yields a healthy stack, on a 4 GB prototype box.

## Status

Foundation / bootstrap. The build is tracked as GitHub issues across five milestones (M0–M4). See the
[open issues](https://github.com/programmism/brainiac/issues) and [milestones](https://github.com/programmism/brainiac/milestones).

## Stack

Go 1.23+ · Postgres 16 + pgvector · Ollama (`nomic-embed-text`) · net/http + chi · pgx · cobra · MCP ·
Docker Compose · Caddy. See [SYSTEM.md §3](SYSTEM.md#3-technology-decisions-and-why) for the rationale
behind each choice.

## License

MIT — see [LICENSE](LICENSE).
