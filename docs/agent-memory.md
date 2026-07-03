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
When you're working within a project, pass its name as `project` to `recall`/`search`
so results focus on that project plus universal facts; omit it to look across everything.

Whenever we learn or decide something worth keeping — a finding from a document, a
conclusion from our discussion, a decision and its rationale, "X works like Y",
"we chose A over B because C" — save it proactively, without being asked:
- `remember` the entities involved (canonical name, type, a short summary), and set
  `project` to the project you're working in (its repo/workspace name) so same-named
  entities in different projects stay distinct. Omit `project` for universal facts
  (a vendor, a standard, a shared tool) — those are global.
- `link` the relationships, always filling in `why`, set source_uri/author when
  known, and pass the same `project` as the entities.
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
One Brainiac serves many projects. How they share and stay distinct is built in
around one idea: **identity** (does a saved entity collide with a same-named one)
and **visibility** (what surfaces on recall) are separate.

- **Global memory (default):** save without a `project` and it's shared — one pool
  for universal facts (a vendor, a standard, a shared tool). Recall without a
  `project` spans everything.
- **Per-project memory (built in):** pass `project` when saving and recalling.
  - **Distinct identity:** `Config` in project *alpha* and `Config` in *beta* are
    two separate entities that accrue their own facts — they never merge, and the
    Consolidate pass won't propose merging them.
  - **Focused recall:** `recall`/`search` with a `project` return that project **+
    global**, not other projects; omit `project` to look across everything.
- **Finer axes (when a project alone isn't enough):** identity takes extra
  discriminators beyond `project` — e.g. `env=prod` vs `env=staging` for the same
  service. Pass them on `remember`/`link` (`discriminators` over MCP, repeatable
  `--disc key=value` on the CLI). Introduce an axis reactively: work with `project`
  until you actually see two things conflated, then add the axis that tells them
  apart. Keep the set small — these are identity keys, not descriptive tags.
- **Per-project isolation (hard, future):** the above is a *soft* lens (nothing is
  hidden — widen by omitting `project`). True separate memories with an enforced
  wall (privacy/compliance) are a future opt-in — see
  [issue #113](https://github.com/programmism/brainiac/issues/113) / #120. Until
  then, run a **separate Brainiac stack** per team if you need hard isolation.

## How it behaves
Tool calls are model-driven: the instruction strongly nudges the agent, but it
decides when to call. You can always steer explicitly — *"check your memory about
X"*, *"save what we just figured out"*, *"why did we choose X?"*. Duplicate entities
can be merged later in the WebUI **Consolidate** tab (or `./brainiac consolidate`).
