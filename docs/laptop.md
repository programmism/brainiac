# Using Brainiac on your laptop

No Go, no exposed ports, no long docker commands — there's a `./brainiac` wrapper,
and docs you drop in `./data/docs` are imported automatically.

## 0. Prerequisites
- Docker Desktop (Mac/Windows/Linux), ~4 GB free RAM.

## 1. Start it
```bash
git clone https://github.com/programmism/brainiac && cd brainiac
cp .env.example .env
./brainiac up
./brainiac logs ollama-pull      # wait for the model to download (~270 MB), then Ctrl-C
```
WebUI: **http://localhost:8080** (Search / Recall / Health).

## 2. Add your docs — just drop files
Put Markdown files under **`./data/docs/`**. With the default `INGEST_INTERVAL=60s`
in `.env`, they're imported automatically within a minute (edits and deletions
too). To import immediately instead of waiting:
```bash
./brainiac import
```
Then search:
```bash
./brainiac search "your question"
./brainiac recall "why does X work this way"
./brainiac health
```

## 2b. Import from Notion (optional)
1. In Notion, create an **internal integration** (Settings → Connections → Develop
   or integrations) and copy its token (`secret_…`).
2. **Share** the pages/databases you want with that integration
   (page → Share → add your integration).
3. Put the token in `.env`:
   ```
   NOTION_TOKEN=secret_xxxxxxxx
   ```
   and restart: `./brainiac down && ./brainiac up`.
4. Import (on demand — Notion isn't polled every minute like local files):
   ```bash
   ./brainiac import --source notion
   ```
   Re-run it whenever you want to sync; it's idempotent (only changed pages
   re-embed). Notion and Markdown land in the same corpus, so search/recall span
   both. (For a schedule, add a cron entry that runs the same command.)

## 3. Connect Claude (capture & recall the "why" via MCP)
```bash
./brainiac mcp-config
```
Paste the printed JSON into Claude Desktop's config
(`~/Library/Application Support/Claude/claude_desktop_config.json` on macOS) and
restart Claude. Keep the stack running. Claude now has `search`, `remember`,
`link`, `recall`, `supersede` — e.g. *"save that A writes to B because …"*, then
later *"why is A built this way?"*.

## Everyday commands
```bash
./brainiac search "…"     ./brainiac recall "…"     ./brainiac health
./brainiac import         # force a re-scan of ./data/docs
./brainiac link --from A --type writes_to --to B --why "…"
./brainiac up | down | logs | mcp-config
```
(Anything after `./brainiac` is passed to the in-container `kb` CLI.)

## Stop / reset
```bash
./brainiac down          # stop (keeps data)
./brainiac down -v       # stop and wipe all data
```

## Notes
- **Windows:** the `./brainiac` script is POSIX sh — use WSL, or run the commands
  directly (`docker compose exec app /kb …`) / the `make` targets.
- **WebUI merge/confirm buttons** are off by default; enable with
  `clients.webui: interactive` + `AUTH_TOKEN` (see `.env.example`). You usually
  don't need them — capture via Claude or the CLI.
- Prefer host-run binaries? `docker compose -f docker-compose.yml -f
  docker-compose.dev.yml up -d` exposes db/ollama on localhost.
