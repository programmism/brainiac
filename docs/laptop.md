# Using Brainiac on your laptop

No Go, no exposed ports — the CLI (`kb`) and MCP server ship inside the container
and are run with `docker compose exec`.

## 0. Prerequisites
- Docker Desktop (Mac/Windows/Linux), ~4 GB free RAM.

## 1. Start the stack
```bash
git clone https://github.com/programmism/brainiac && cd brainiac
cp .env.example .env
docker compose up -d
# First boot downloads the embedding model (~270 MB):
docker compose logs -f ollama-pull      # wait for it to finish, then Ctrl-C
```
Open the WebUI at **http://localhost:8080** (Search / Recall / Health). It's empty
until you add knowledge.

## 2. Load your docs (semantic search / RAG)
Put Markdown files under `./data` (e.g. `./data/docs/`), then:
```bash
docker compose exec app /kb import --source markdown --path /data/docs
docker compose exec app /kb health          # counts, index size, connectivity
```
Now search from the WebUI, or:
```bash
docker compose exec app /kb search "your question"
docker compose exec app /kb recall "why does X work this way"
```

## 3. Capture & recall the "why" from Claude (MCP)
This is the primary workflow — tell Claude *"save what we found…"* and ask
*"why is X…"* later. Point Claude Desktop at the in-container MCP server.

Add to your Claude Desktop MCP config
(`~/Library/Application Support/Claude/claude_desktop_config.json` on macOS):
```json
{
  "mcpServers": {
    "brainiac": {
      "command": "docker",
      "args": ["compose", "-f", "/ABSOLUTE/PATH/TO/brainiac/docker-compose.yml",
               "exec", "-T", "app", "/brainiac-mcp"]
    }
  }
}
```
Replace the path, restart Claude Desktop. The stack must be running
(`docker compose up -d`). Claude now has the tools: `search`, `remember`, `link`,
`recall`, `supersede`.

## 4. (Optional) Enable the WebUI merge/confirm buttons
Writes are off by default. To turn them on, set `clients.webui: interactive` in a
`config.yaml`, set `AUTH_TOKEN=...` in `.env`, and send `Authorization: Bearer
<token>` with write calls. For a laptop you usually don't need this — capture via
Claude/MCP or the CLI (`kb link`, `kb remember`).

## 5. (Optional) Run host binaries instead of docker-exec
```bash
docker compose -f docker-compose.yml -f docker-compose.dev.yml up -d   # exposes db + ollama on localhost
go build -o bin/ ./cmd/...
DATABASE_URL='postgres://brainiac:brainiac@localhost:5432/brainiac?sslmode=disable' \
OLLAMA_URL='http://localhost:11434' ./bin/cli health
```

## Stop / reset
```bash
docker compose down          # stop (keeps data)
docker compose down -v       # stop and wipe all data
```
