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
  -d '{"repo":"alice/widgets","prNumber":42,"headSha":"abc1234","labels":["agentic-review-default"],"diff":"..."}'

curl 'http://127.0.0.1:4819/reviews?repo=alice/widgets&pr=42&label=default'   # status + last result (label key)
```

How it behaves:

- **One workflow per PR** (`review:<repo>#<n>`): webhook retries and new pushes are delivered as signals to the same execution via signal-with-start, so reviews never duplicate; a push that lands mid-review triggers a fresh review of the newest head.
- **Activities are retried** (4 attempts, exponential backoff), **heartbeat** while the agent runs, and respect a **diff budget** (`review.maxDiffBytes`, default 500000 bytes when the key is omitted). `FetchDiff` reads at most one byte past that budget. Anything longer is skipped with an explicit reason and is not sent to the reviewer; a read that stops on the cap is not the remote diff's full size, and that prefix is never reviewed as a complete diff. An inline diff is skipped the same way using its real length. No environment variable overrides this number.
- **Timeouts**: `review.reviewerTimeout` (default `12m`) bounds each reviewer subprocess and is passed to it as `CURSOR_REVIEW_TIMEOUT`; `review.activityTimeout` (default `15m`) is the `RunReviewAgent` StartToClose and must be at least 1m longer than the reviewer timeout (validated at startup). Heartbeats every 10s with a 1m heartbeat timeout; `FetchDiff`/`PostReview` keep a 10m StartToClose.
- **Pluggable reviewer**: `review.reviewerCommand` gets `{"repo","prNumber","title","diff"}` on stdin and must print `{"summary","findings":[{file,line,severity,message}]}` on stdout. `cmd/mock-reviewer` is deterministic and needs no credentials. `cmd/cursor-reviewer` (`/app/cursor-reviewer` in the image) runs the headless Cursor CLI: `agent --print --output-format json --model $CURSOR_REVIEW_MODEL --workspace <tmp> --trust --approve-mcps --disable-auto-update`, read-only permissions (`.cursor/cli.json` denies Shell/Write/WebFetch), and the MCP gateway wired via `.cursor/mcp.json` with `${env:GATEWAY_REVIEWER_KEY}` so no key is written to disk. Env: `CURSOR_API_KEY` (required), `CURSOR_REVIEW_MODEL` (default `grok-4.7`), `CURSOR_REVIEW_TIMEOUT` (default `12m`; when run by the orchestrator it is set from `review.reviewerTimeout`), `AGENTFLOW_GATEWAY_URL` (`-` disables the gateway). Exit codes: 0 ok, 1 agent failure (retried), 2 bad input / 3 invalid JSON (non-retryable).
- **Pluggable sink**: `review.sink.type` is `file` (JSON per PR under `reviews/`) or `github` (requires `GITHUB_TOKEN`). The GitHub sink keeps **one comment per PR**, found by the `<!-- agentflow-review -->` marker and PATCHed in place on retries and new pushes; the comment carries the reviewed head SHA. GitHub 401/403/404/422 (not rate limits) are non-retryable.
- **Concurrency**: `worker.maxConcurrentActivities` (default 4) caps concurrent activities per worker pod, so at most 4 Cursor CLI processes run at once; extra PRs queue in Temporal. Scale out by adding replicas.
- **Label-gated reviews**: only PRs carrying a review label are reviewed (`review.labels`; default `agentic-review-default` -> the default model `grok-4.7` / `$CURSOR_REVIEW_MODEL` / `review.defaultModel`, and `agentic-review-claude` -> `claude-opus-5-5-high`; `$CURSOR_REVIEW_MODEL_<KEY>` overrides a rule's model). Unlabeled PRs are skipped. Each (PR, label) pair has its own workflow `review:<repo>#<n>:<key>`, its own upserted comment (the default label uses `<!-- agentflow-review -->`, others `<!-- agentflow-review:<key> -->`) that names the model (`<!-- agentflow-model: ... -->`), and is reviewed once per (head SHA, model): when the label is first seen and on each new push while it stays. The model is passed per workflow to the reviewer (stdin `model` and `CURSOR_REVIEW_MODEL`). The webhook applies the same gating via a `labels` array in the body. With the 15m poll interval, a newly added label is picked up within ~15 minutes.
- **PR poller** (outbound only; no public webhook needed): with `poller.enabled`, the orchestrator idempotently creates the Temporal Schedule `poller.scheduleId` (default `pr-poller`, every `poller.interval` = 15m, overlap Skip) running `PRPollerWorkflow`. It lists open PRs for `poller.owner` (all non-archived repos, filtered by `poller.repos` / `poller.excludeRepos`; drafts included unless `includeDrafts` is false), and signal-with-starts a review per review label only when the (head SHA, model) differs from the last reviewed one (workflow status query, including a closed run still in retention, then that label's marker comment). A finished **diff-budget skip** of the same head and model is queued again only when `review.maxDiffBytes` is strictly greater than the budget recorded in that skip. The same budget is not retried on later polls. An in-progress review and a successful review are not repeated because the budget changed. Uses ETags and stops when the GitHub rate limit runs low.
- Set `WEBHOOK_TOKEN` to require an `X-Webhook-Token` header on the webhook; leave it unset only on localhost.

Configuration lives in `orchestrator.config.json` (Temporal host/namespace/task queue, webhook host/port, review settings). Tests: `go test ./...` exercises the workflow against Temporal's test suite.

Which file loads: a CLI path argument, otherwise `ORCHESTRATOR_CONFIG`, otherwise `./orchestrator.config.json`. Inside that file, an explicit `review.maxDiffBytes` replaces the compiled default of 500000 (used only when the key is omitted). The shipped root config and `deploy/k8s/orchestrator.config.json` both set 500000. `ORCHESTRATOR_CONFIG` and `/etc/agentflow/orchestrator.env` (tokens such as `GITHUB_TOKEN` and `CURSOR_API_KEY`) do not change the byte count.

The Kubernetes pod reads ConfigMap `orchestrator-config` key `config.json` at `/etc/orchestrator/config.json` (the image sets `ORCHESTRATOR_CONFIG`) once, at startup. Replacing the ConfigMap and restarting the orchestrator Deployment applies a new `review.maxDiffBytes` to fetches made by the **running** binary. Retrying a skip that already finished, and the operator commands below, are code in the orchestrator binary: they take effect only after an image built from this commit is running. Worker concurrency (default 4), the 12m reviewer / 15m activity timeouts, label rules, and the allowlist are separate settings and stay put.

### Operator: kick one poll

Manual runs go through the poller schedule. They do not call `/hooks/pr`.

```bash
kubectl -n agent-gateway exec deploy/orchestrator -- /app/orchestrator trigger-poller
```

That triggers schedule `pr-poller` with overlap **Skip**. The command prints JSON and exits 0 for `accepted` (a poll started) or `running` (one is already in progress, including a trigger that coalesced onto it; nothing new was started). Exit 2 is `skipped` (the schedule is paused, or a trigger coalesced without a poll still running). Exit 1 is `error`. The poll that starts is a normal poll: allowlist, labels, models, and the dedupe rules above, including a one-time budget-skip retry when the loaded budget is higher.

To retry one previously skipped pull without waiting for the interval, name the repo, the PR, and that PR's **current** 40-character head:

```bash
kubectl -n agent-gateway exec deploy/orchestrator -- \
  /app/orchestrator trigger-poller retry-skipped owner/repo PR_NUMBER FULL_HEAD_SHA
```

This still uses the poller's GitHub credential, allowlist, draft setting, and label rules. It starts a review only for a label whose latest finished result for that exact head and model was a diff-budget skip, and only when the loaded `review.maxDiffBytes` is greater than the budget in that skip. A successful review is left alone. A review already in progress is reported as `running` and is not signaled again. History and the existing comment are not deleted or edited by the command; a started review posts through the normal sink when it finishes. The same exit codes apply. `orchestrator trigger-poller` with any other arguments is rejected.

## Deployment

One multi-stage `Dockerfile` builds all four static binaries into a single distroless image (gateway is the entrypoint; the orchestrator deployment overrides `command`). Two supported paths:

### Option 1: Kubernetes (`deploy/k8s/`)

```bash
docker build -t <your-registry>/agent-gateway:0.1.0 .
docker push <your-registry>/agent-gateway:0.1.0
```

**Gateway** (tool plane):

```bash
kubectl create namespace agent-gateway
kubectl -n agent-gateway create configmap gateway-config \
  --from-file=config.json=deploy/k8s/gateway.config.json
kubectl -n agent-gateway create secret generic gateway-keys \
  --from-literal=GATEWAY_ADMIN_KEY="$(openssl rand -hex 32)" \
  --from-literal=GATEWAY_REVIEWER_KEY="$(openssl rand -hex 32)"
# set the image in deploy/k8s/manifests.yaml, then:
kubectl apply -f deploy/k8s/manifests.yaml
```

**Orchestrator** (control plane) — needs a reachable Temporal cluster first (official Helm chart: `helm repo add temporal https://temporalio.github.io/helm-charts && helm install temporal temporal/temporal -n temporal --create-namespace`), then:

```bash
# edit deploy/k8s/orchestrator.config.json: temporal.hostPort, sink type
kubectl -n agent-gateway create configmap orchestrator-config \
  --from-file=config.json=deploy/k8s/orchestrator.config.json
kubectl -n agent-gateway create secret generic orchestrator-tokens \
  --from-literal=WEBHOOK_TOKEN="$(openssl rand -hex 24)" \
  --from-literal=GITHUB_TOKEN="<your-pat-or-empty>"
# set the image in deploy/k8s/orchestrator.yaml, then:
kubectl apply -f deploy/k8s/orchestrator.yaml
```

Point your forge's PR webhook at the orchestrator Service (`/hooks/pr`, with the `X-Webhook-Token` header). Both container configs bind 0.0.0.0, read secrets from env, write runtime data to an `emptyDir` at `/data`, and expose `/healthz` for probes. Services are ClusterIP (the MCP gateway is internal-only; the reviewer reaches it at `http://agent-gateway.agent-gateway.svc.cluster.local/mcp`); expose the orchestrator on your tailnet with the [Tailscale Kubernetes operator](https://tailscale.com/kb/1236/kubernetes-operator) or an ingress of your choice.

### Option 2: systemd on a plain server (`deploy/systemd/`)

For a single home server, skip k8s entirely: three units with install steps in their header comments.

```bash
go build -o /opt/agentflow/gateway ./cmd/gateway
go build -o /opt/agentflow/demo-upstream ./cmd/demo-upstream
go build -o /opt/agentflow/orchestrator ./cmd/orchestrator
go build -o /opt/agentflow/mock-reviewer ./cmd/mock-reviewer
# then follow the comments in:
#   deploy/systemd/temporal-dev.service    single-node Temporal, SQLite persistence
#   deploy/systemd/agent-gateway.service   tool plane on :4817
#   deploy/systemd/orchestrator.service    control plane on :4819
```

Secrets (gateway API keys, `WEBHOOK_TOKEN`, `GITHUB_TOKEN`, `CURSOR_API_KEY` for a real reviewer) go in `/etc/agentflow/*.env` files referenced by the units, not in the configs.

## Notes

- The gateway is stateless per request, so it sits happily behind a reverse proxy; terminate TLS there (Caddy/Traefik/nginx) before exposing it off-host.
- A failed upstream doesn't take the gateway down — it's reported in `/healthz` and its tools simply disappear until it reconnects (restart the gateway to retry).
