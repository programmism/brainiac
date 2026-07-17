# Screenshots for the docs

The first-run tutorial ([../first-run.md](../first-run.md)) references these
screenshots. They must be captured on a live deploy (the WebUI isn't rendered in
CI), so they're tracked here as a checklist rather than committed placeholders:

- [ ] `first-run-health.png` — WebUI **Health** tab: db ok / embedder ok.
- [ ] `first-run-recall.png` — **Recall** tab for "why does OrderService use Kafka", showing the edge `why`.
- [ ] `first-run-graph.png` — **Graph** tab: the `OrderService → Kafka` edge.
- [ ] `first-run-mcp.png` — Claude Desktop with the brainiac MCP tools connected.

Capture at ~1400px wide, light theme, and drop the PNGs in this folder; the
tutorial already points at these filenames.
