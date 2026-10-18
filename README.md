# agent-gateway

A self-hosted **remote agent platform for code review**, written in **Go**, with two components:

1. **The agent gateway** (`cmd/gateway`) — the tool/data plane: a lightweight stand-in for AWS AgentCore Gateway. One authenticated MCP endpoint aggregating your upstream MCP servers with per-key tool policies and audit logging.
2. **The orchestrator** (`cmd/orchestrator`) — the control plane: a [Temporal](https://temporal.io) workflow that turns PR webhooks into agent review runs with retries, heartbeats, budget guards, re-review on new pushes, and queryable status.

## The gateway (tool/data plane)

It exposes **one authenticated MCP endpoint** that your agents (Cursor CLI, Cursor Cloud Agents, Temporal activities, anything that speaks MCP over Streamable HTTP) connect to, and it:

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
cmd/gateway/              gateway entrypoint: load config, connect upstreams, listen
cmd/demo-upstream/        bundled demo MCP server (echo, current_time, sha256, generate_uuid)
cmd/smoke/                gateway end-to-end smoke test (auth, policy, round-trip)
cmd/orchestrator/         control plane: Temporal worker + PR webhook receiver
cmd/mock-reviewer/        deterministic stand-in review agent (no API key needed)
internal/gateway/         HTTP handler, auth, and the per-principal MCP server
internal/upstream/        upstream MCP client manager (stdio + streamable HTTP)
internal/policy/          allow-pattern matching and tool-name namespacing
internal/audit/           JSONL audit logger
internal/config/          gateway config schema, env substitution, validation
internal/orchestrator/    CodeReviewWorkflow, activities, config, tests
```

Built on the official [MCP Go SDK](https://github.com/modelcontextprotocol/go-sdk).

## The orchestrator (control plane)

The orchestrator runs two things in one binary: a Temporal **worker** hosting `CodeReviewWorkflow`, and a **webhook receiver** that starts (or signals) one workflow per PR.

```bash
temporal server start-dev          # or your self-hosted Temporal cluster
go run ./cmd/orchestrator          # webhook on http://127.0.0.1:4819
```

Trigger a review (the request carries the diff inline, or a `diffUrl` the workflow fetches):

```bash
curl -X POST http://127.0.0.1:4819/hooks/pr -H 'Content-Type: application/json' \
  -d '{"repo":"alice/widgets","prNumber":42,"headSha":"abc1234","diff":"..."}'

curl 'http://127.0.0.1:4819/reviews?repo=alice/widgets&pr=42'   # status + last result
```

How it behaves:

- **One workflow per PR** (`review:<repo>#<n>`): webhook retries and new pushes are delivered as signals to the same execution via signal-with-start, so reviews never duplicate; a push that lands mid-review triggers a fresh review of the newest head.
- **Activities are retried** (4 attempts, exponential backoff), **heartbeat** while the agent runs, and respect a **diff budget** (`review.maxDiffBytes`, default 300 KB) so a webhook storm or giant vendored diff can't drain your usage pool.
- **Pluggable reviewer**: `review.reviewerCommand` gets `{"repo","prNumber","title","diff"}` on stdin and must print `{"summary","findings":[{file,line,severity,message}]}` on stdout. The default is the bundled deterministic `cmd/mock-reviewer` so everything runs without credentials. For production, point it at a script that invokes the headless Cursor CLI, e.g. `agent -p "Review this diff... output only JSON: ..." --model grok-4.7 --force=false`, with the gateway configured in `mcp.json` and `CURSOR_API_KEY` set.
- **Pluggable sink**: `review.sink.type` is `file` (JSON per PR under `reviews/`) or `github` (posts a formatted PR comment; requires `GITHUB_TOKEN`).
- Set `WEBHOOK_TOKEN` to require an `X-Webhook-Token` header on the webhook; leave it unset only on localhost.

Configuration lives in `orchestrator.config.json` (Temporal host/namespace/task queue, webhook host/port, review settings). Tests: `go test ./...` exercises the workflow against Temporal's test suite.

## Deploy to Kubernetes

A multi-stage `Dockerfile` (distroless, static binary, runs as nonroot) and manifests under `deploy/k8s/` are included:

```bash
docker build -t <your-registry>/agent-gateway:0.1.0 .
docker push <your-registry>/agent-gateway:0.1.0

kubectl create namespace agent-gateway
kubectl -n agent-gateway create configmap gateway-config \
  --from-file=config.json=deploy/k8s/gateway.config.json
kubectl -n agent-gateway create secret generic gateway-keys \
  --from-literal=GATEWAY_ADMIN_KEY="$(openssl rand -hex 32)" \
  --from-literal=GATEWAY_REVIEWER_KEY="$(openssl rand -hex 32)"
# set the image in deploy/k8s/manifests.yaml, then:
kubectl apply -f deploy/k8s/manifests.yaml
```

The container config (`deploy/k8s/gateway.config.json`) binds 0.0.0.0, reads API keys from the `gateway-keys` Secret via `${ENV}` substitution, and writes the audit log to an `emptyDir` at `/data`. Readiness and liveness probes hit `/healthz`. The Service is ClusterIP; expose it on your tailnet with the [Tailscale Kubernetes operator](https://tailscale.com/kb/1236/kubernetes-operator) or an ingress of your choice.

## Notes

- The gateway is stateless per request, so it sits happily behind a reverse proxy; terminate TLS there (Caddy/Traefik/nginx) before exposing it off-host.
- A failed upstream doesn't take the gateway down — it's reported in `/healthz` and its tools simply disappear until it reconnects (restart the gateway to retry).
