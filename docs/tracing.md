# AgentFlow OpenTelemetry tracing (Honeycomb)

Traces are a **debug projection**. Dedupe, evals and review records stay on Temporal + the file/GitHub sink. This change does not add an eval runner or Langfuse.

## Chosen base and code gap

| Item | Detail |
|---|---|
| GitHub `main` | Orchestrator webhook + mock reviewer only. **No** poller, Cursor CLI reviewer, label gating, or 12m/15m timeouts. |
| Implementation base | `feature/cursor-reviewer` @ `1acf4bb` (open PR #1). Manifests tagged **0.2.3**. |
| Last known live | **0.2.4**: Temporal 15m poller, GitHub **and Origin** repos, label gating, Cursor CLI reviewer timeout 12m, activity 15m. |
| Gap vs live 0.2.4 | This GitHub branch has GitHub-only polling (no Origin client). `agentflow.forge` defaults to `github` so Origin can be stamped later without a schema break. GitHub may lag the cluster; do not merge/revert unrelated PR #1 review fixes. |
| Honeycomb `test` | Confirmed empty of AgentFlow data (only fpl-web / movie-postcredits / battleships-bot). Last poll cannot be determined until this is deployed and enabled. |

## What is emitted

Short completed spans (plus Temporal per-attempt `RunActivity` spans once the interceptor is on):

- Poll: `agentflow.poller.run`, `agentflow.scan.repo`, `agentflow.dedupe.check`, `agentflow.poller.start_target`
- Cycle markers: `agentflow.cycle.started` / `.finished` / `agentflow.review.outcome` (zero-duration; start markers are flushed immediately)
- Review attempts: `agentflow.attempt.started` / `.finished`, `agentflow.diff.fetch`, `invoke_agent agentflow-reviewer`, `agentflow.reviewer.{prepare,subprocess,parse}`, `agentflow.comment.upsert`
- Gateway: `execute_tool` (principal **name**, policy, ok, duration — no args/results)
- CLI subprocess: `agentflow.cli.run` from documented `json` envelope fields only (`session_id`, durations). No fabricated `gen_ai.usage.*`. Stream-json is **not** wired.

Poller traces **link** to review cycles; they do not parent them. Correlation is `agentflow.repo` / `pr.number` / `head_sha` / `gen_ai.request.model` / `agentflow.review.key`. Workflow code only logs cycle start/finish (replay-safe). Unfinished long spans are **not** treated as exported.

Failure classes: `rate_limit`, `usage_limit`, `timeout.reviewer`, `timeout.activity`, `killed`, `cli_error`, `parse.envelope`, `parse.verdict`, `bad_input`, `diff_fetch`, `post.github`, `gateway.*`.

## Enable (backward-compatible no-op)

Tracing stays off unless an OTLP endpoint is set.

**Environment (preferred in-cluster):**

```bash
OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector.agent-gateway.svc.cluster.local:4318
OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf
OTEL_SERVICE_NAME=orchestrator          # cursor-reviewer / agent-gateway set their own
OTEL_SERVICE_VERSION=0.2.4             # image tag
AGENTFLOW_ENVIRONMENT=test             # → deployment.environment
AGENTFLOW_RUN_KIND=prod
# AGENTFLOW_TRACING=0                  # force no-op even if an endpoint is set
```

**Config** (`orchestrator.config.json`, optional):

```json
"tracing": {
  "enabled": true,
  "otlpEndpoint": "http://otel-collector.agent-gateway.svc.cluster.local:4318",
  "environment": "test",
  "runKind": "prod"
}
```

Broken exporter: batch processor, short flush timeouts, errors logged; reviews continue.

## k3s / k8s activation

1. Create the Honeycomb **test** ingest-key secret (placeholder only in git):

   ```bash
   kubectl -n agent-gateway create secret generic honeycomb \
     --from-literal=HONEYCOMB_API_KEY='<PLACEHOLDER_TEST_INGEST_KEY>'
   ```

2. Apply collector: `kubectl apply -f deploy/k8s/otel-collector-config.yaml -f deploy/k8s/otel-collector.yaml`

3. Add the `OTEL_*` / `AGENTFLOW_ENVIRONMENT=test` env vars to the orchestrator and gateway deployments (see comments in `deploy/k8s/orchestrator.yaml` and `deploy/k8s/manifests.yaml`). Rebuild/push the image from this branch. **Do not** add `agentic-review-*` labels as part of activation.

4. Roll the orchestrator: `kubectl -n agent-gateway rollout restart deploy/orchestrator deploy/agent-gateway`

5. Smoke: trigger one poll, then run the last-poll query below.

```bash
kubectl -n agent-gateway exec deploy/orchestrator -- /app/orchestrator trigger-poller
kubectl -n agent-gateway logs deploy/otel-collector --tail=50
```

Expect orchestrator log `pr-poller run` and collector logs without auth errors. In Honeycomb **test**, dataset = `service.name` (`orchestrator`, `cursor-reviewer`, `agent-gateway`).

## Honeycomb last-poll query (test)

VISUALIZE: `COUNT`, `MAX(timestamp)`  
WHERE: `name = agentflow.poller.run`  
GROUP BY: none (or `deployment.environment`)  
time range: last 24h, dataset `orchestrator` (or all remaining if you query across datasets)

If that is empty, the poller is not exporting (collector down, env not set, or this image not rolled). A row's `MAX(timestamp)` **is** the last successful poll-attempt span end. Scan counts live on that span: `agentflow.poller.repos`, `.open_prs`, `.unlabeled`, `.targets`. Per-repo: `name = agentflow.scan.repo`.

Silent reviews: `COUNT WHERE name = agentflow.review.outcome GROUP BY agentflow.outcome, gen_ai.request.model`. Undetermined attempts: compare `agentflow.attempt.started` vs `agentflow.attempt.finished` grouped by `agentflow.review.key`.

## Rollback

1. Set `AGENTFLOW_TRACING=0` on orchestrator/gateway **or** delete `OTEL_EXPORTER_OTLP_ENDPOINT`, restart.
2. `kubectl -n agent-gateway delete deploy/otel-collector svc/otel-collector` (optional). Leave the honeycomb secret in place.
3. Image rollback to the previous tag (live 0.2.4) does not require this PR; tracing is additive.

Do not rotate keys from this change. Do not force-push.

## Privacy

Never on spans/logs: `CURSOR_API_KEY`, gateway/GitHub/Origin/webhook tokens, Authorization headers, raw diffs, prompts, finding messages, tool args/results, webhook bodies. Collector redaction is a backstop; in-process sanitizer + synthetic sentinel tests cover the same patterns.
