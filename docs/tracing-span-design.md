# AgentFlow tracing span design (OpenTelemetry GenAI + eval compatibility)

Status: proposal (docs only, no code changes in this PR)
Scope: `PRPollerWorkflow`, `CodeReviewWorkflow`, the `FetchDiff`, `RunReviewAgent` and `PostReview` activities, `cmd/cursor-reviewer` (Cursor CLI subprocess), and the MCP gateway (`internal/gateway`).

## 0. Principles

1. **Traces are a projection.** The source of truth is the Temporal workflow result plus a persisted **review record** (§11). Evals, dedupe and reporting read the record. They never read Honeycomb or Langfuse. Traces are there for humans and debugging. (This follows the idea in https://dbln.me/blog/building-an-ai-agent-observability-first.)
2. **Vendor-neutral.** Use OTel SDK, OTLP and the GenAI semantic conventions (`gen_ai.*`, v1.40+, Development status, so opt in with `OTEL_SEMCONV_STABILITY_OPT_IN=gen_ai_latest_experimental`). Everything AgentFlow-specific goes under `agentflow.*`, and eval-specific fields go under `eval.*`.
3. **Model only what we can observe.** We cannot see the Cursor CLI's internal model calls, prompts-as-sent or token usage (§7). We observe what the CLI prints, what the gateway serves and what we parse. We do not invent `chat` spans with made-up tokens.
4. **Short spans, early export.** A 12-minute review should show data within seconds. That means many short child spans plus one span per Temporal activity attempt. A crashed attempt still leaves its earlier children behind, and its own end state reads as **undetermined**, never as success.

## 1. Trace boundaries and IDs

| Trace | Root span | Lifetime |
|---|---|---|
| Poller run | `PRPollerWorkflow` run (one per `pr-poller` schedule fire, every 15m, overlap Skip) | ~seconds |
| Review cycle | one `CodeReviewWorkflow` **cycle**: one (sha, model) pass of the loop, fetch → review → post | ~2.5m typical, up to 4 × 15m |

- The poller trace does **not** parent the review trace. `StartReviewTargets` does `SignalWithStartWorkflow`, and a running workflow may absorb the signal into a later cycle. So the poller's `agentflow.poller.start_target` span gets an **OTel span link** to the review cycle, and the review cycle root links back to the poller span that triggered it (via the signal/start header context).
- **One trace per review cycle, not per workflow.** The workflow ID `review:<repo>#<pr>:<labelKey>` (or `review:origin:<repo>#<pr>:<labelKey>`) is reused across pushes and executions (`ALLOW_DUPLICATE`). A trace per cycle keeps each (sha, model) review small and separable. A new-push signal mid-cycle starts a new cycle trace that links to the previous one.
- **Trace ID ≠ correlation ID.** The trace ID is random per cycle and is never derived from repo/PR/sha. That way a replay, retry-with-new-execution or eval rerun never merges into an old trace. Correlation happens through attributes that appear on **every** span in the cycle (use a `BaggageSpanProcessor` or set them explicitly):

| Attribute | Example |
|---|---|
| `agentflow.repo` | `hash167/agentflow` |
| `agentflow.forge` | `github` \| `origin` |
| `agentflow.pr.number` | `1` |
| `agentflow.pr.head_sha` | `7241547…` (full 40 chars) |
| `agentflow.label_key` / `agentflow.label` | `claude` / `agentic-review-claude` |
| `gen_ai.request.model` | `claude-opus-5-5-high` |
| `agentflow.review.key` | `hash167/agentflow#1@<sha>:<model>` (the dedupe key) |
| `temporal.workflow.id` / `temporal.run.id` | from the Temporal interceptor / `workflow.GetInfo` |
| `agentflow.cycle` | loop index inside the execution (0, 1, …) |
| `agentflow.run_kind` | `prod` \| `eval` |
| `gen_ai.conversation.id` | = `agentflow.review.key` for prod, or `eval.run_id/eval.case_id` for eval. Honeycomb's Agent Timeline groups by this. |
| `gen_ai.agent.name` | `agentflow-reviewer` |

Attempt-level IDs (`temporal.activity.attempt`, `temporal.activity.id`) appear only on activity spans and their children.

## 2. Span tree

The Temporal interceptor (`go.temporal.io/sdk/contrib/opentelemetry`, `NewTracingInterceptor` registered on both client and worker) produces the `StartWorkflow:*`, `RunWorkflow:*`, `StartActivity:*`, `RunActivity:*` and `SignalWithStartWorkflow:*` spans. It creates **one `RunActivity` span per attempt**. Everything below `RunActivity` is manual.

```
[trace A] poller run
RunWorkflow:PRPollerWorkflow                               agentflow.poller.*
├─ RunActivity:ListOpenPRs (attempt n)
│  ├─ agentflow.scan.repo  (one per repo; GitHub hash167/* and Origin, mirrors skipped)
│  │    attrs: repo, forge, prs_open, prs_labeled, skipped_reason=mirror|origin_excluded
│  └─ http.client GET …/pulls  (otelhttp)
├─ RunActivity:LastReviewedTargets
│  └─ agentflow.dedupe.check  (one per target; source=workflow_query|comment_marker|none, decision=skip|review)
└─ RunActivity:StartReviewTargets
   └─ agentflow.poller.start_target  (one per target)  ──link──▶ trace B root
        └─ SignalWithStartWorkflow:CodeReviewWorkflow

[trace B] one review cycle  (gen_ai.operation.name=invoke_workflow)
invoke_workflow agentflow-review          (root; manual, started in workflow code per cycle)
├─ RunActivity:FetchDiff (attempt 1..4)
│  └─ agentflow.diff.fetch   http GET (github) / origin files API
├─ RunActivity:RunReviewAgent (attempt 1..4)        ← one span per attempt (interceptor)
│  └─ invoke_agent agentflow-reviewer               (gen_ai.operation.name=invoke_agent)
│     ├─ agentflow.reviewer.prepare     (workdir, cli.json, mcp.json, prompt render)
│     ├─ agentflow.reviewer.subprocess  (exec of cursor-reviewer; pid, exit code)
│     │   └─ [in cursor-reviewer process, parented via TRACEPARENT]
│     │      agentflow.cli.run          (exec of `agent --print …`; CLI session_id)
│     │      ├─ agentflow.cli.init               event/short span from stream `system/init`
│     │      ├─ agentflow.cli.assistant_message  span event per `assistant` message (length, hash; no text by default)
│     │      ├─ execute_tool <tool>              one span per `tool_call started→completed` (call_id)
│     │      │   └─ [gateway] execute_tool <upstream>__<tool>   (parented via MCP header, §6)
│     │      │        └─ agentflow.gateway.upstream_call
│     │      └─ agentflow.cli.result              from terminal `result` (duration_ms, is_error, request_id)
│     ├─ agentflow.reviewer.parse       (ParseCLIOutput + ExtractVerdict + Validate)
│     │    events: agentflow.finding × N   (§11.3)
│     └─ agentflow.reviewer.heartbeat   span event per heartbeat (10s ticker in RunReviewAgent; sample to 1 per 30s)
├─ RunActivity:PostReview (attempt 1..4)
│  └─ agentflow.comment.upsert  (find marker → create|update; or file sink / sandbox)
└─ agentflow.review.outcome   (zero-duration closing span; carries the outcome union, §4)
```

Notes:
- The cycle root is started in **workflow code** and must be replay-safe. Use the interceptor's workflow tracer (`workflow.Context`-aware) so replays do not emit duplicate spans. Close it with the outcome before `return` or `continue`.
- Heartbeats already run on a 10s ticker in `RunReviewAgent`. Mirror them (sampled to one per 30s) as span events so a hung attempt visibly stops "ticking".
- Until the CLI runs with `--output-format stream-json`, `agentflow.cli.run` has no CLI-event children (§7). The tree still works.

## 3. Attributes per span

| Span | Key attributes |
|---|---|
| `RunWorkflow:PRPollerWorkflow` | `agentflow.poller.schedule_id=pr-poller`, `.repos`, `.open_prs`, `.unlabeled`, `.targets`, `.triggered`, `.unchanged` (from `PollSummary`) |
| `agentflow.scan.repo` | `agentflow.repo`, `agentflow.forge`, `agentflow.scan.prs_open`, `.prs_labeled`, `.skipped_reason` |
| `agentflow.dedupe.check` | correlation set + `agentflow.dedupe.source`, `.decision`, `.prev_sha`, `.prev_model` |
| cycle root `invoke_workflow agentflow-review` | full correlation set, `gen_ai.operation.name=invoke_workflow`, version stamps (§11.2), **outcome union** (§4), `agentflow.findings.count`, `.findings.by_severity.{error,warning,info}` |
| `RunActivity:*` (interceptor) | `temporal.activity.type`, `temporal.activity.attempt`, `temporal.workflow.id`, `temporal.run.id`. Add via a small worker interceptor: `agentflow.attempt.max=4`, `agentflow.timeout.start_to_close_s`, `agentflow.timeout.heartbeat_s` |
| `agentflow.diff.fetch` | `agentflow.diff.bytes`, `.files`, `.sha256`, `.truncated`, `http.response.status_code`, `agentflow.diff.over_budget` (vs `maxDiffBytes`=300000) |
| `invoke_agent agentflow-reviewer` | `gen_ai.operation.name=invoke_agent`, `gen_ai.agent.name`, `gen_ai.provider.name=cursor`, `gen_ai.request.model`, `gen_ai.response.model` (from CLI `system/init.model` if streamed), `agentflow.reviewer.timeout_s=720`, `agentflow.reviewer.gateway_enabled`, `agentflow.reviewer.mode`, version stamps |
| `agentflow.reviewer.subprocess` | `process.pid`, `process.exit.code`, `agentflow.reviewer.exit_class` (`ok`/`agent_failed`/`bad_input`/`invalid_output`/`killed`), `agentflow.reviewer.stderr_tail` (≤2 KB, redacted, §8) |
| `agentflow.cli.run` | `agentflow.cli.version`, `agentflow.cli.session_id`, `agentflow.cli.request_id`, `agentflow.cli.duration_ms`, `agentflow.cli.duration_api_ms`, `agentflow.cli.is_error`, `agentflow.cli.output_format`, `agentflow.cli.tool_calls`, `agentflow.cli.assistant_messages` |
| `execute_tool <tool>` (CLI side) | `gen_ai.operation.name=execute_tool`, `gen_ai.tool.name` (`readToolCall`, or `function.name` such as `agentflow-gateway-…`), `gen_ai.tool.call.id` (= CLI `call_id`), `agentflow.tool.ok`, `.result_chars`. Arguments only as a hash or short path |
| `execute_tool <upstream>__<tool>` (gateway) | `gen_ai.tool.name`, `agentflow.gateway.upstream`, `.principal` (key **name**, never the value), `.policy` (`allowed`/`denied`), `agentflow.tool.ok`, `.duration_ms`. Same fields as the audit `Entry` |
| `agentflow.reviewer.parse` | `agentflow.parse.stage` (`envelope`/`verdict`/`validate`), `agentflow.parse.candidates`, `agentflow.findings.count`, `error.type` on failure |
| `agentflow.comment.upsert` | `agentflow.sink` (`github`/`origin`/`file`/`sandbox`), `agentflow.comment.action` (`created`/`updated`/`skipped_eval`), `agentflow.comment.marker`, `agentflow.comment.id`, `http.response.status_code` |

Token usage: the Cursor CLI's documented output has no token counts. Leave `gen_ai.usage.*` **unset**. Do not set it to 0, because 0 would produce wrong cost estimates downstream. Cost for evals comes from wall time and Cursor usage reports out of band (`agentflow.cost.source=unavailable|cursor_dashboard`).

## 4. End states and the outcome union

Every cycle root and `invoke_agent` span ends with **exactly one** `agentflow.outcome`:

| `agentflow.outcome` | Meaning | OTel status |
|---|---|---|
| `reviewed` | valid verdict with ≥1 finding, posted | OK |
| `reviewed_clean` | valid verdict with 0 findings, posted | OK |
| `skipped` | `Skipped` set (empty diff, over budget), `agentflow.skip_reason` | OK |
| `superseded` | new-push signal arrived, so the result is replaced by the next cycle | OK |
| `failed` | terminal error, `agentflow.error.class` set (§5) | ERROR |
| `usage_limited` | provider/Cursor usage or quota limit (today's Opus failure) | ERROR |
| `timed_out` | reviewer 12m kill or activity StartToClose/heartbeat timeout | ERROR |
| `undetermined` | span ended without a known outcome (worker crash, SIGKILL, process exit) | UNSET + `agentflow.undetermined=true` |

How each case gets there:
- **Crashed attempt.** The `RunActivity` span from a crashed worker is never exported, which is a gap. To make it explicit, start the per-attempt `invoke_agent` span immediately and **force-flush its start marker**: a zero-duration `agentflow.attempt.started` span exported right away. If no `agentflow.attempt.finished` with the same `temporal.activity.attempt` ever arrives, the attempt is undetermined. The next attempt records `agentflow.previous_attempt_outcome=undetermined` from `activity.GetInfo(ctx).Attempt > 1` plus the last heartbeat details. In Honeycomb: `COUNT WHERE name=agentflow.attempt.started` minus `finished`, grouped by `agentflow.review.key`.
- **Timeouts.** The reviewer's 12m `context.WithTimeout` gives `agentflow.error.class=timeout.reviewer`. Temporal StartToClose (15m) or a heartbeat timeout (1m) shows up as the next attempt's `temporal.activity.attempt>1` and `agentflow.previous_attempt_outcome=timeout.activity`. The cycle root records `timed_out` only if retries are exhausted.
- **Usage limits.** Per the current code, the CLI fails non-zero, `cursor-reviewer` returns exit 1 (retryable), the attempt is retried 4× against the same limit, and the cycle fails. The design: classify the stderr or the `result.is_error` text (§5), give the reviewer a dedicated exit code (e.g. `4 = usage_limited`), and map it to a **non-retryable** `ReviewerUsageLimited` ApplicationError, or to a long-backoff retry. Either way it becomes the `usage_limited` outcome, never a silent success.
- **"Failed reviews get recorded as done."** The dedupe check reads `status.HeadSHA`, which is set at cycle start. So a failed cycle blocks a re-review of the same sha. The outcome union belongs in the workflow `Status` and the review record so dedupe can skip only `reviewed*`/`skipped` outcomes. Traces only *show* this. The fix lives in the record.

## 5. Error taxonomy (`agentflow.error.class`)

| Class | Source signal | Retry? |
|---|---|---|
| `rate_limit` | HTTP 429 or "rate limit" in CLI stderr/`result`; GitHub/Origin 403 + `x-ratelimit-remaining: 0` | yes, with backoff |
| `usage_limit` | CLI error text naming a usage, quota, plan or credit limit (e.g. today's Opus error) | no (or next window) → outcome `usage_limited` |
| `timeout.reviewer` | reviewer `runCtx` DeadlineExceeded (12m) | yes |
| `timeout.activity` | Temporal StartToClose / heartbeat timeout | yes (Temporal) |
| `killed` | subprocess killed by signal (SIGKILL on cancel), `agentflow.reviewer.orphan_risk=true` | yes |
| `cli_error` | CLI non-zero exit or `is_error=true` not matching the above | yes |
| `parse.envelope` | `ParseCLIOutput`: no JSON `result` envelope | no (exit 3) |
| `parse.verdict` | `ExtractVerdict`/`Validate` failed | no (exit 3) |
| `bad_input` | exit 2 (empty diff, missing `CURSOR_API_KEY`) | no |
| `diff_fetch` | `FetchDiff` HTTP 401/404 (`DiffFetchPermanent`) or other | 401/404 no, else yes |
| `post.github` / `post.origin` | `UpsertReviewComment` failure (`asTemporalError`) | per status |
| `gateway.auth` / `gateway.policy_denied` / `gateway.upstream` | gateway audit events | tool-level, not fatal |

Set `error.type` (low-cardinality, = class) and `exception.message` (truncated to 512 B, redacted) per the OTel recording-errors spec. Propagate tool failures to the parent's status only when they make the attempt fail.

## 6. Context propagation

1. **Poller to workflow.** The Temporal tracing interceptor injects context into the `SignalWithStartWorkflow` header. Because the workflow may already be running, treat the incoming context as a **link**, not a parent, on the cycle root.
2. **Workflow to activity.** The interceptor propagates through activity headers, giving one `RunActivity` span per attempt as a child of the cycle root.
3. **Activity to cursor-reviewer.** `RunReviewAgent` sets `cmd.Env` with `TRACEPARENT`/`TRACESTATE` (W3C, from the `invoke_agent` span) plus `OTEL_EXPORTER_OTLP_ENDPOINT` and `OTEL_SERVICE_NAME=cursor-reviewer`. `cursor-reviewer` extracts with `propagation.TraceContext{}` from the env carrier and exports its own spans.
4. **cursor-reviewer to Cursor CLI.** The CLI has no documented OTel support, so it is a black box. We read its stdout (§7) and create spans *on its behalf* in `cursor-reviewer`.
5. **CLI to gateway.** The CLI connects to the gateway with headers from `.cursor/mcp.json`, which already uses `${env:GATEWAY_REVIEWER_KEY}` interpolation. Add `"traceparent": "${env:TRACEPARENT}"` (and optionally `"x-agentflow-review-key"`) to the headers. The gateway's HTTP handler extracts it (otelhttp), so every `tools/call` becomes a child of the CLI-run span. This is per run, not per tool call, which is fine for correlation. Verify with one run that the CLI actually sends interpolated custom headers.
6. **Gateway to upstreams.** Use otelhttp on the upstream client. Inject `traceparent` into upstream requests only for first-party upstreams.

Security: do **not** pass `os.Environ()` wholesale (an existing finding). Build an explicit env allowlist (`PATH`, `HOME`, `CURSOR_API_KEY`, `GATEWAY_REVIEWER_KEY`, `CURSOR_REVIEW_*`, `TRACEPARENT`, `TRACESTATE`, `OTEL_*`). That way `GITHUB_TOKEN`, `WEBHOOK_TOKEN` and others never reach the CLI.

## 7. Cursor CLI observability (what is really there)

- The reviewer runs `agent --print --output-format json --model <m> --workspace <ws> --trust --disable-auto-update [--approve-mcps] [--mode]`.
- `json` output: **one** final object `{type:"result", subtype:"success", is_error, duration_ms, duration_api_ms, result, session_id, request_id?}`. On failure it gives a non-zero exit and stderr text, with **no** JSON object. (Source: https://cursor.com/docs/cli/reference/output-format)
- `stream-json` output: NDJSON events. These are `system/init` (`model`, `session_id`, `apiKeySource`, `permissionMode`, `cwd`), `user` (the prompt), one `assistant` per complete message, `tool_call` `started`/`completed` with `call_id` and typed payloads (`readToolCall`, `writeToolCall`, or `function{name,arguments}`), and the terminal `result`. On failure the stream may end early with no `result`. `--stream-partial-output` adds deltas. `thinking` events are suppressed in print mode. **No token or cost fields are documented.**
- Proposal: switch to `--output-format stream-json`. Tee stdout line by line, emit spans/events as lines arrive (data shows up within seconds), and keep `ParseCLIOutput`'s "last line is the result envelope" logic, which still holds.
- The CLI is not installed on this box, so `agent --help` could not be checked here. Fields are from the docs page above. Pin the expectations in a test against the image's CLI version (`2026.10.22`, per the comment in `Args`).

## 8. What NOT to put in spans

- **Never:** `CURSOR_API_KEY`, `GATEWAY_REVIEWER_KEY`, `GITHUB_TOKEN`, `ORIGIN_TOKEN`, `WEBHOOK_TOKEN`, `Authorization` headers, or the env dump. Add a collector `attributes`/`redaction` processor that deletes `*.authorization`, `*token*` and `*api_key*` keys and masks `ghp_…`, `github_pat_…`, `crsr_…`-style patterns as a backstop.
- **No full diffs.** Record `agentflow.diff.bytes`, `.sha256` and `.files`. Optionally include `agentflow.diff.preview` capped at 4 KB. Honeycomb rejects events over 1 MB and caps string fields at 64 KB, and the diff budget alone is 300 KB.
- **Prompt/response bodies are off by default.** `gen_ai.input.messages`/`gen_ai.output.messages` are opt-in (env `AGENTFLOW_TRACE_CONTENT=1`, eval runs only), capped at 32 KB each, and replaced by `agentflow.prompt.sha256` + `agentflow.prompt.bytes` otherwise. Full prompt/output goes to the review record artifact (§11), not the span.
- **No tool-result content.** Record lengths and hashes only.
- Keep `stderr_tail` at ≤2 KB and redacted. Keep attribute cardinality sane: findings are events, not attributes named per finding.

## 9. Export path

```
orchestrator worker ┐
cursor-reviewer     ├─OTLP/gRPC or HTTP─▶ otel-collector (in-cluster) ─┬─▶ Honeycomb (OTLP, x-honeycomb-team: ingest key from Secret)
agent-gateway       ┘                     batch, memory_limiter,      └─▶ (optional) Langfuse /api/public/otel  (OTLP/HTTP only, no gRPC)
                                          redaction, tail filter          filter: gen_ai.* spans + run_kind=eval
```

- Use one collector deployment. Apps only know `OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector.<ns>:4318`, so Honeycomb's ingest key lives only in the collector Secret.
- Dataset: `service.name` per binary in one environment (e.g. `agentflow-test`). Use resource attributes `deployment.environment`, `service.version` (= image tag).
- `cursor-reviewer` must call `ForceFlush` before `os.Exit` (it is short-lived), with a ~2s bounded timeout.
- For Langfuse later, add a second exporter plus a `filter` processor. Map `agentflow.run_kind=eval` to `langfuse.environment=experiment` and `eval.*` to `langfuse.experiment.*` with a `transform` processor (§11.5).

## 10. Example Honeycomb queries

1. **Silent failures by model:** `COUNT WHERE name="invoke_workflow agentflow-review" AND agentflow.run_kind=prod GROUP BY gen_ai.request.model, agentflow.outcome`, over the last 7d.
2. **Usage-limit incidents:** `COUNT WHERE agentflow.error.class=usage_limit GROUP BY gen_ai.request.model, agentflow.label_key` as a Trigger (alert if >0 in 15m).
3. **Latency per model:** `HEATMAP(duration_ms), P50, P95 WHERE name="invoke_agent agentflow-reviewer" AND agentflow.outcome IN (reviewed, reviewed_clean) GROUP BY gen_ai.request.model`.
4. **Retry waste:** `COUNT WHERE name="RunActivity:RunReviewAgent" GROUP BY temporal.activity.attempt, agentflow.error.class`.
5. **Undetermined attempts:** `COUNT WHERE name="agentflow.attempt.started"` vs `"agentflow.attempt.finished"` `GROUP BY agentflow.review.key` (HAVING started > finished).
6. **Gateway tool usage and denials:** `COUNT, P95(duration_ms) WHERE gen_ai.operation.name=execute_tool GROUP BY gen_ai.tool.name, agentflow.gateway.policy`.
7. **Time to first signal:** `MIN(timestamp)` of the first `agentflow.cli.*` child relative to the `invoke_agent` start. This shows CLI cold-start cost.
8. **Eval comparison (debug view only):** `AVG(eval.score.recall) WHERE agentflow.run_kind=eval GROUP BY eval.experiment, gen_ai.request.model`. The authoritative numbers come from the eval store (§11).

## 11. Eval compatibility

### 11.1 Separate eval runs from prod
- `agentflow.run_kind=prod|eval` on every span (resource attribute and baggage).
- Eval spans also carry `eval.dataset` (e.g. `seeded-bugs-v1`), `eval.case_id` (stable per seeded PR), `eval.run_id` (unique per execution of one case × config), and `eval.experiment` (e.g. `2026-11-03-prompt-v3-vs-v2`).
- **Every eval run gets a fresh trace ID.** `eval.case_id` is a correlation key, never a trace key, so replays and reruns don't merge. Eval workflow IDs are namespaced: `eval:<experiment>:<case_id>:<model>:<run_id>`, so they never collide with or dedupe against `review:*` IDs.
- `gen_ai.conversation.id = eval.run_id` for eval runs, which gives one Agent Timeline conversation per run.

### 11.2 Version stamps (on cycle root, `invoke_agent`, and the record)
| Attribute | How |
|---|---|
| `agentflow.prompt.template_hash` | sha256 of `reviewer.Prompt()` rendered with **placeholder** input (`{{repo}}`, `{{pr}}`, `{{title}}`, `{{diff_file}}`, gateway on/off). This hashes the template, not the rendered prompt. Also add a human `agentflow.prompt.template_version` constant |
| `gen_ai.request.model` / `gen_ai.response.model` | label rule model / CLI `system/init.model` |
| `agentflow.reviewer.version` | `cursor-reviewer` build version (ldflags) |
| `agentflow.cli.version` | `agent --version` captured at reviewer start (cached) |
| `agentflow.image.tag` / `service.version` | from deploy manifest env |
| `agentflow.config.hash` | sha256 of canonical JSON of the effective `ReviewConfig` + reviewer env (timeout, mode, gateway on/off, extra args; secrets excluded) |
| `agentflow.diff.sha256` and `agentflow.pr.head_sha` | content hash and git sha |

### 11.3 Findings as stable structured records
Each validated finding becomes one span event `agentflow.finding` on `agentflow.reviewer.parse` and one entry in the record:

```json
{ "id": "f_<sha256(file|line|severity|category|message_hash)[:12]>",
  "file": "internal/orchestrator/activities.go", "line": 182,
  "severity": "error", "category": "security|correctness|concurrency|error_handling|data_loss|maintainability|other",
  "message_hash": "<sha256(normalized message)[:16]>", "message": "<only in record; span gets message_preview ≤200 chars>" }
```
- `category` needs a prompt or schema change: add an optional `category` to the verdict JSON, and default to `other` in `Validate`.
- Scorers match findings to seeded bugs by (file, line ± tolerance, category), with the message as an LLM-judge fallback. The `id` is stable across reruns of the same output, which allows exact-duplicate detection across models.

### 11.4 Outcome union
The record and the cycle root carry the same `agentflow.outcome` (§4). Scorers must distinguish `reviewed_clean` (a true "no findings") from `failed`/`usage_limited`/`timed_out`/`undetermined`. Those are excluded from recall/FP and counted as **availability** failures instead.

### 11.5 Scores written back (linked, not authoritative)
- The eval runner computes scores from records, then emits **one span per score set**, `agentflow.eval.score`, in its own trace with an **OTel link** to the review cycle's trace/span ID (stored in the record).
- It also emits `gen_ai.evaluation.result` events (OTel GenAI events spec) on that span, with `gen_ai.evaluation.name` (`recall`, `false_positives`, `severity_accuracy`, `latency_s`, `cost_usd`), `gen_ai.evaluation.score.value`, and `gen_ai.evaluation.score.label`. Honeycomb renders these in the Span Events tab, and in the Gen AI tab when the span has `gen_ai.operation.name`.
- Langfuse mapping later: each score becomes a Langfuse score (`name`, `value`, `dataType` NUMERIC/BOOLEAN/CATEGORICAL) referencing `traceId`, or `datasetRunId` for run-level aggregates. Eval spans get `langfuse.experiment.id/name/dataset.id` and `langfuse.experiment.item.id = eval.case_id` via collector transform.

### 11.6 Hard rule: evals read the record, never the trace backend
- **Where:** (a) the `CodeReviewWorkflow` result / `Status.LastResult` in Temporal (bounded by retention), and (b) a durable JSON artifact written by `PostReview` *before* the comment upsert. Today's `file` sink (`<forge_>repo-pr<N>[-<label>].json`) already does this for dev. For prod and eval, write `reviews/<run_kind>/<review.key or eval.run_id>.json` to a PVC or object store, append-only (one file per cycle, not overwritten).
- **Schema `agentflow.review_record/v1`** (superset of today's `ReviewResult`):

```json
{ "schema": "agentflow.review_record/v1",
  "run_kind": "prod|eval", "eval": {"dataset":"", "case_id":"", "run_id":"", "experiment":""},
  "repo": "", "forge": "", "pr_number": 0, "head_sha": "", "label_key": "", "model": "",
  "workflow_id": "", "run_id": "", "cycle": 0, "attempts": 1,
  "trace_id": "", "root_span_id": "",
  "versions": {"prompt_template_hash":"", "prompt_template_version":"", "reviewer":"", "cli":"", "image":"", "config_hash":"", "diff_sha256":""},
  "outcome": "reviewed|reviewed_clean|skipped|superseded|failed|usage_limited|timed_out|undetermined",
  "error": {"class":"", "message":""},
  "summary": "", "findings": [ /* §11.3 */ ],
  "timing": {"started_at":"", "ended_at":"", "review_wall_ms":0, "cli_duration_ms":0, "cli_duration_api_ms":0},
  "cost": {"source":"unavailable", "usd": null},
  "cli": {"session_id":"", "request_id":""},
  "posted": {"sink":"github|origin|file|sandbox|none", "comment_id":"", "url":""} }
```
- The same file is the input to scorers, the dedupe fallback (outcome-aware), and the trace link (`trace_id`).

### 11.7 Eval-mode switch
- `ReviewRequest` gets `RunKind` + `Eval{Dataset,CaseID,RunID,Experiment}`. When `RunKind=eval`, `PostReview` **never** calls GitHub/Origin comment APIs. It either uses `sink=file` (already implemented) or `sink=sandbox` (a dedicated `hash167/agentflow-eval-sandbox` repo whose PRs mirror the corpus), and it records `agentflow.comment.action=skipped_eval` otherwise.
- Eval runs never carry `agentic-review-*` labels and never go through `PRPollerWorkflow`. They come from an `EvalRunWorkflow` (scheduled, or triggered on a prompt-template-hash/model/config change) that fans out `CodeReviewWorkflow` children per (case × model × prompt version) and respects the 4-activity concurrency cap.
- Corpus: seed from today's 13 Claude findings, stored as `eval/corpus/<case_id>/{diff.patch, expected.json}`, where expected entries are (file, line range, category, severity).

## 12. Phased plan

1. **Phase 0, record first (no OTel yet):** outcome union in `Status`/`ReviewResult`; outcome-aware dedupe; usage-limit classification + exit code; the env allowlist; the process-group kill fix; and `review_record/v1` artifacts.
2. **Phase 1, base tracing:** OTel SDK + collector + Honeycomb exporter; Temporal tracing interceptor on client and worker; cycle root span with correlation attributes and version stamps; manual spans for diff fetch, subprocess, parse and comment upsert; redaction processor.
3. **Phase 2, reviewer and gateway:** `TRACEPARENT` into `cursor-reviewer`; ForceFlush; `traceparent` header in `mcp.json`; otelhttp + `execute_tool` spans in the gateway (mirroring audit entries); attempt started/finished markers for undetermined detection.
4. **Phase 3, CLI events:** switch to `stream-json`; incremental `execute_tool`/assistant-message spans; heartbeat events. Verify against the pinned CLI version.
5. **Phase 4, evals:** `RunKind`/eval-mode sink; `EvalRunWorkflow`; corpus from the 13 seeds; scorer reading records; score spans + `gen_ai.evaluation.result` events; Honeycomb boards and triggers (§10).
6. **Phase 5, optional:** a Langfuse exporter behind a collector filter, with the experiment/score mapping. Langfuse is for human review only. Records stay the source of truth.
