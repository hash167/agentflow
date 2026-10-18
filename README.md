# agent-gateway

A self-hosted **MCP agent gateway** written in **Go** — a lightweight stand-in for AWS AgentCore Gateway that you can run on your own servers (single static binary, no runtime dependencies). It exposes **one authenticated MCP endpoint** that your agents (Cursor CLI, Cursor Cloud Agents, Temporal activities, anything that speaks MCP over Streamable HTTP) connect to, and it:

- **Aggregates upstream MCP servers** (stdio or HTTP) behind a single URL, namespacing tools as `<upstream>__<tool>`.
- **Authenticates callers** with per-agent API keys (`Authorization: Bearer <key>`).
- **Enforces tool policies per key** — each key gets an allowlist (`"*"`, `"github:*"`, `"github:get_pr"`), so a code-review agent only ever sees the tools you gave it.
- **Audit-logs every tool call** (and every denial and auth failure) as JSONL with principal, tool, outcome, and latency.

Secrets for upstream servers (API tokens, OAuth headers) live only in the gateway's config/environment — agents never see them.

## Run it

Requires Go 1.25+ (older Go toolchains auto-download the right version).

```bash
go run ./cmd/gateway          # starts on http://127.0.0.1:4817/mcp with the demo upstream
```

In a second terminal:

```bash
go run ./cmd/smoke            # end-to-end test: auth, aggregation, policy enforcement
```

For deployment, build static binaries:

```bash
go build -o bin/gateway ./cmd/gateway
go build -o bin/demo-upstream ./cmd/demo-upstream
```

## Configuration

Everything is in `gateway.config.json` (or pass a path: `go run ./cmd/gateway ./my-config.json`, or set `GATEWAY_CONFIG`). The shipped config uses the bundled `demo-tools` stdio upstream and two dev keys so it works with zero setup:

```jsonc
{
  "host": "127.0.0.1",          // set to "0.0.0.0" to expose on your LAN
  "port": 4817,
  "auditLog": "./audit.jsonl",
  "apiKeys": [
    { "name": "admin", "key": "${GATEWAY_ADMIN_KEY}", "allow": ["*"] },
    { "name": "code-reviewer", "key": "${GATEWAY_REVIEWER_KEY}", "allow": ["github:get_pull_request", "demo-tools:*"] }
  ],
  "upstreams": [
    { "name": "demo-tools", "transport": { "type": "stdio", "command": "go", "args": ["run", "./cmd/demo-upstream"] } },
    { "name": "github", "transport": { "type": "http", "url": "https://api.githubcopilot.com/mcp/", "headers": { "Authorization": "Bearer ${GITHUB_PAT}" } } }
  ]
}
```

- `${VAR}` anywhere in keys, headers, or stdio env is substituted from the gateway's environment at startup (and startup fails loudly if the variable is missing).
- Replace the dev keys before exposing the gateway beyond localhost: `openssl rand -hex 32` makes a good key.

## Point an agent at it

**Cursor CLI / editor** — add to `~/.cursor/mcp.json` (or a repo's `.cursor/mcp.json`):

```json
{
  "mcpServers": {
    "home-gateway": {
      "url": "http://your-server:4817/mcp",
      "headers": { "Authorization": "Bearer <your-key>" }
    }
  }
}
```

**Cursor Cloud Agents API** — pass the same URL + header as an inline MCP server when creating an agent.

**Temporal code-review pipeline** — give each workflow's agent its own key with only the tools that review stage needs; the audit log then gives you a per-principal trace of every tool call across the pipeline.

## Endpoints

| Endpoint | Description |
|---|---|
| `POST /mcp` | The MCP endpoint (Streamable HTTP, stateless). Requires `Authorization: Bearer <key>`. |
| `GET /healthz` | Gateway + per-upstream connection status. |
| `GET /` | Service info. |

## Layout

```
cmd/gateway/              entrypoint: load config, connect upstreams, listen
cmd/demo-upstream/        bundled demo MCP server (echo, current_time, sha256, generate_uuid)
cmd/smoke/                end-to-end smoke test (auth, policy, round-trip)
internal/gateway/         HTTP handler, auth, and the per-principal MCP server
internal/upstream/        upstream MCP client manager (stdio + streamable HTTP)
internal/policy/          allow-pattern matching and tool-name namespacing
internal/audit/           JSONL audit logger
internal/config/          config schema, env substitution, validation
```

Built on the official [MCP Go SDK](https://github.com/modelcontextprotocol/go-sdk).

## Notes

- The gateway is stateless per request, so it sits happily behind a reverse proxy; terminate TLS there (Caddy/Traefik/nginx) before exposing it off-host.
- A failed upstream doesn't take the gateway down — it's reported in `/healthz` and its tools simply disappear until it reconnects (restart the gateway to retry).
