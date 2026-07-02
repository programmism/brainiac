# Making Brainiac an agent's memory

Brainiac is a **long-term memory any MCP agent can share** — it consults it for
advice and saves new findings itself. It's not Claude-specific: MCP is an open
protocol, so Claude Desktop, Claude Code, Cursor, Cline, VS Code, or your own
agent (built on any MCP SDK) can all use it.

Two ingredients:
1. **Connect** the `brainiac` MCP server to the agent.
2. **Instruct** the agent (globally or per-project) to *use* the memory — recall
   before answering, and save findings/decisions as they happen.

## 1. Connect the MCP server
The server runs inside the container; agents launch it over stdio:
```
command: docker
args:    ["compose","-f","/ABSOLUTE/PATH/TO/brainiac/docker-compose.yml","exec","-T","app","/brainiac-mcp"]
```
`./brainiac mcp-config` prints this JSON. Put it wherever your agent registers MCP
servers:
- **Claude Desktop / Claude Code:** `claude_desktop_config.json` (or `claude mcp add`).
- **Cursor / Cline / VS Code / others:** their MCP servers config — same `command`+`args`.
- **Custom agent:** any MCP client SDK; spawn the same command as a stdio server.

(Everything runs in-container, so no Go and no exposed ports. A network/HTTP MCP
transport for remote agents is a future addition; today it's stdio.)

Tools exposed: `recall`, `search`, `remember`, `link`, `supersede`, `add_document`
(store text the agent read elsewhere), and `ingest` (bulk import via a connector).

## 2. The memory instruction (paste into the agent's system prompt / rules)
Use it **globally** (applies to every conversation) or per-project:
- **Claude:** account-level *Custom Instructions* = global; a *Project*'s custom
  instructions = scoped to that project.
- **Claude Code:** `~/.claude/CLAUDE.md` = global; `./CLAUDE.md` = per-repo.
- **Cursor/Cline/etc.:** global rules vs per-workspace rules.

```
You have a persistent memory provided by the `brainiac` MCP server. Treat it as
your long-term memory, shared across all sessions.

Before answering anything non-trivial (how or why something works, a decision, a
fact about our systems, projects, or past choices), first call `recall` (or
`search`) to check what is already known, and ground your answer in it — cite the
source_uri of each claim. If nothing relevant is found, say so briefly, then answer.

Whenever we learn or decide something worth keeping — a finding from a document, a
conclusion from our discussion, a decision and its rationale, "X works like Y",
"we chose A over B because C" — save it proactively, without being asked:
- `remember` the entities involved (canonical name, type, a short summary).
- `link` the relationships, always filling in `why`, and set source_uri/author
  when known.
- For a document or passage that should be findable verbatim later (e.g. a page
  you read via your own integration), call `add_document` with a stable source_uri
  and the text.
- If a previous decision changed, call `supersede` — never delete; keep the history.

Save decisions, rationale, and non-obvious findings; skip small talk and trivia.
Keep entity names canonical and consistent so duplicates merge. When unsure whether
something is already stored, `recall` first to avoid duplicates. At the end of a
substantive session, briefly list what you saved.
```
`./brainiac instructions` prints this block so you can pipe it into a rules file.

## Global vs per-project memory
- **Global memory (today, out of the box):** one shared corpus. Everything every
  agent saves goes into one pool — ideal as your personal/team-wide brain.
- **Per-project memory (soft convention, today):** keep one Brainiac but *scope by
  naming*. In a project's instructions add: *"This memory is for project **Alpha**:
  prefix source_uris with `alpha/…`, add a `project: Alpha` note to what you save,
  and prefer Alpha facts when recalling."* It's a nudge, not enforced isolation.
- **Per-project isolation (hard, future):** true separate memories per project/team
  = **namespaces** (a `namespace` column + scoped ops). Not built yet — see
  [issue #113](https://github.com/programmism/brainiac/issues/113) and PRD §21
  (multi-team isolation vs shared graph). Until then, run a **separate Brainiac
  stack** per project if you need hard isolation.

## How it behaves
Tool calls are model-driven: the instruction strongly nudges the agent, but it
decides when to call. You can always steer explicitly — *"check your memory about
X"*, *"save what we just figured out"*, *"why did we choose X?"*. Duplicate entities
can be merged later in the WebUI **Consolidate** tab (or `./brainiac consolidate`).
