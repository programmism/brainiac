# Deployment & operations

## Quickstart (dev)
```bash
cp .env.example .env
docker compose up
```
Brings up Postgres (pgvector), Ollama (+ model pull), and the app (auto-migrates, serves on `:8080`).

## Production: reverse proxy + auth (Caddy)
The MCP server is stdio-only and never network-exposed. The app's HTTP surface (WebUI + REST) goes
behind Caddy, which adds TLS + basic auth. See [`../Caddyfile`](../Caddyfile).

1. Generate a password hash:
   ```bash
   docker run --rm caddy:2 caddy hash-password --plaintext 'your-password'
   ```
2. Set in `.env`:
   ```
   SITE_ADDRESS=memory.example.com   # domain → auto-HTTPS. Or ":443" (add `tls internal`), or ":80".
   BASIC_AUTH_USER=admin
   BASIC_AUTH_HASH=$2a$14$...        # the hash from step 1
   ```
3. Start with the proxy profile:
   ```bash
   docker compose --profile proxy up -d
   ```
4. **Do not expose the app or Postgres directly.** In production, remove the `app` service's host port
   mapping so only Caddy (80/443) is reachable; Postgres stays on the internal network.

## Security defaults
- The app binds to **host-localhost only** (`127.0.0.1:8080`) — not the LAN. Production exposes it solely
  through Caddy (proxy profile).
- **Write endpoints** (`/api/merge`, `/api/edges/{id}/confirm|flag-stale`, the WebUI merge/confirm
  buttons) are **disabled by default**. To enable them:
  1. set `clients.webui: interactive` in `config.yaml`;
  2. set a strong `AUTH_TOKEN` in `.env`;
  then send `Authorization: Bearer <AUTH_TOKEN>` with write requests. Reads stay open — protect them with
  the Caddy proxy.

## Scaling on the compute axis — GPU (#252)
Brainiac runs fine on a 4 GB CPU box (the base compose caps Ollama at 1.5 GB / 2
CPUs for the small embedder). To go faster — bigger corpora, higher search QPS, or
the optional local-LLM extractor (`EXTRACTOR=local-llm`, which loads a chat model
into the same Ollama) — put Ollama on a **GPU** with the override:

```bash
docker compose -f docker-compose.yml -f docker-compose.gpu.yml up -d
docker compose exec ollama nvidia-smi   # confirm the GPU is visible in-container
```

Requires an NVIDIA GPU + driver and the **NVIDIA Container Toolkit** on the host.
The override reserves the GPU for Ollama and lifts the CPU-box memory/CPU caps
(tune `mem_limit`/`cpus` in `docker-compose.gpu.yml` to your hardware). Nothing
else changes — the app, DB, migrations, and `./brainiac` commands are identical.
See also the sizing notes in [production-readiness.md](production-readiness.md).

## Backups
See [operations: backup & restore](operations.md).
