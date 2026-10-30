# ContextForge spike (#14)

Throwaway setup behind [ADR 0001](../../docs/adr/0001-mcp-gateway-contextforge.md).

```bash
spikes/contextforge/run.sh               # Postgres + ContextForge v1.1.0 + two test MCP servers (namespace mcp-spike)
# wire projects through the ContextForge API (see the ADR): register gateways, create virtual servers,
# mint a token per agent scoped to its server, save them to $JCODE_SCRATCH_DIR/cf-<agent>.{tok,sid}
spikes/contextforge/check-agent.sh alice # claude in alice's pod lists only project-alpha tools
spikes/contextforge/run.sh teardown
```
Generated secrets live in `.secrets/contextforge/` (gitignored).
