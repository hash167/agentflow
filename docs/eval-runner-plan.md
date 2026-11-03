# AgentFlow lean eval runner: plan

Status: proposal (docs only). Builds on the tracing span design in [`docs/tracing-span-design.md`](./tracing-span-design.md), which is on PR #2's branch (https://github.com/hash167/agentflow/pull/2) and not yet on main. Section references like "span design §11" point there.
Decision (Hashim): Honeycomb stays the tracing backend. Evals use a lean in-house runner. Langfuse may come later, but only as another collector exporter.

## 1. Goals and non-goals

**Goals**
- Measure the **real** reviewer path (`RunReviewAgent` → `cursor-reviewer` → Cursor CLI, same prompt and config) against a fixed corpus of PRs with known bugs.
- Compare models (`grok-4.7`, `claude-opus-5-5-high`) and prompt versions side by side, with repeats to handle nondeterminism.
- Catch regressions automatically when the prompt, model or reviewer config changes, using a stored baseline and explicit gates.
- Keep results as durable JSON (the source of truth) and project them into Honeycomb for humans.

**Non-goals (for now)**
- No Langfuse, Braintrust or other eval SaaS, and no UI beyond Honeycomb boards and a Markdown summary.
- No LLM-as-judge in v1. It is a later, optional phase (§6.4).
- No token or $ cost metric, because the CLI reports no tokens (span design §7). Wall time is the proxy.
- No evaluation of production PRs, which have no ground truth. Prod traffic is only *observed*.
- No prompt or model auto-tuning.

## 2. Corpus format

```
evals/
  datasets/
    seeded-bugs-v1.json          # dataset manifest: list of case_ids + dataset version
  cases/
    <case_id>/
      case.json                  # metadata (below)
      base.ref | base/           # either a pinned sandbox SHA, or omitted for patch-only cases
      head.patch                 # unified diff the reviewer sees (what FetchDiff would return)
      expected.json              # seeded bugs (empty list for clean controls)
      NOTES.md                   # optional: why this case exists, provenance
```

`case.json`
```json
{ "schema": "agentflow.eval_case/v1",
  "case_id": "orch-failed-review-recorded-done",
  "kind": "seeded | clean_control | full_pr",
  "source": {"type": "pr_finding", "repo": "hash167/agentflow", "pr": 1, "head_sha": "1acf4bb354b952e9c5e3eb0fd35f5f4417f4ae66", "base_sha": "b54cbdbdca3eb12257416a1395dd4ae22bfe3e7c", "finding_index": 0},
  "diff": {"mode": "patch | sandbox", "patch": "head.patch",
           "sandbox": {"repo": "hash167/agentflow-eval-sandbox", "base_sha": "…", "head_sha": "…"}},
  "title": "PR title shown to the reviewer",
  "tags": ["orchestrator", "temporal"],
  "added": "2026-11-03" }
```

`expected.json`
```json
{ "schema": "agentflow.eval_expected/v1",
  "bugs": [
    { "bug_id": "B1",
      "file": "internal/orchestrator/workflow.go",
      "lines": [76, 90],
      "category": "correctness",
      "min_severity": "warning",
      "expected_severity": "error",
      "priority": "must_find",
      "description": "Head SHA recorded as reviewed before the review succeeds",
      "aliases": [{"file": "internal/orchestrator/poller.go", "lines": [290, 305]}] } ],
  "allowed_extra": [ {"file": "Dockerfile", "category": "supply_chain"} ]
}
```

- `lines` is the new-file line range where a hit counts.
- `priority` is `must_find` or `nice_to_find`.
- `category` is one of `security|correctness|concurrency|resource_leak|error_handling|supply_chain|performance|compat|maintainability`.
- A hit below `min_severity` counts as a severity miss.
- `allowed_extra` lists true findings that weren't seeded, so they aren't counted as FPs.

- **Patch mode** is the default. `head.patch` is fed directly as `ReviewRequest.Diff`, so `FetchDiff` is skipped, the case is fully reproducible, and no GitHub calls are needed.
- **Sandbox mode** uses a pinned SHA pair in a private `agentflow-eval-sandbox` repo. It is for cases that need the gateway or MCP tools to read surrounding files. The diff is fetched via the normal `FetchDiff` path.

### 2.1 Seeding from PR #1 (head `1acf4bb`)
The 13 `claude-opus-5-5-high` findings on PR #1 (raw JSON at `/workspace/origin-agentflow1-comments.json`, one Origin comment with marker `agentflow-review:claude`, base `b54cbdb`) become the first ground truth:

| # | Sev | Location | Category | Priority |
|---|---|---|---|---|
| 1 | error | `internal/orchestrator/workflow.go:81`: HeadSHA/Model recorded before review succeeds | correctness | must_find |
| 2 | warning | `internal/orchestrator/poller.go:298`: dedupe ignores failed/unfinished cycles | correctness | must_find (alias of #1) |
| 3 | error | `internal/orchestrator/activities.go:151`: SIGKILL orphans CLI process group | resource_leak | must_find |
| 4 | warning | `internal/reviewer/reviewer.go:376`: agent inherits full env (GITHUB_TOKEN, WEBHOOK_TOKEN) | security | must_find |
| 5 | warning | `internal/orchestrator/github.go:207`: marker comment from any author is trusted | security | must_find |
| 6 | warning | `internal/orchestrator/activities.go:64`: token attached to any webhook diffUrl under API base | security | must_find |
| 7 | warning | `internal/orchestrator/poller.go:268`: one target's permanent error fails the whole batch | error_handling | nice_to_find |
| 8 | warning | `internal/orchestrator/github.go:162`: ETag cache never evicted | resource_leak | nice_to_find |
| 9 | info | `internal/orchestrator/poller.go:216`: `RateRemaining` read without lock | concurrency | nice_to_find |
| 10 | warning | `Dockerfile:21`: unpinned `curl … \| bash` install | supply_chain | nice_to_find |
| 11 | info | `cmd/orchestrator/main.go:74`: poller shares 4 activity slots with reviews | performance | nice_to_find |
| 12 | info | `internal/orchestrator/config.go:115`: hardcoded default owner | maintainability | nice_to_find |
| 13 | info | `cmd/orchestrator/main.go:127`: webhook behavior change for unlabeled requests | compat | nice_to_find |

Steps, done once by a script, `evals/tools/seed-from-comment`:
1. Parse the comment with the `- **sev** \`file:line\` — message` lines. Write one `expected.json` entry per finding, with a line window of ±8 around the cited line, plus category and priority from the table above (the human review step).
2. **Full-PR case** `pr1-full-1acf4bb`: `head.patch = git diff b54cbdb..1acf4bb` (~120 KB, under the 300 KB `maxDiffBytes` budget), and all 13 bugs in `expected.json`. This measures realistic recall on a large diff.
3. **Focused cases**, one per must-find bug: a small patch (≤300 lines) against the post-fix code that re-introduces just that bug, with 1 expected bug each. This is less diluted and gives cleaner per-bug signal. Write these after the PR #1 fixes land, because they need the fixed code as the base.
4. **Human pass:** Hashim confirms each finding is real. Anything rejected is dropped or moved to `allowed_extra`.

Caveat: these seeds come from Claude's own review, so v1 favors what Claude already finds. Grow the set with bugs from other sources (§2.3) to reduce that bias.

### 2.2 Clean controls (false positives)
- `clean-*` cases with `bugs: []`:
  - (a) the PR #1 **fix** diffs once merged, which are correct code touching the same areas
  - (b) pure refactors, renames and doc-only diffs
  - (c) a test-only diff
  - (d) a large but benign diff (vendored or generated file) to test noise under size
- Aim for roughly 1 clean control per 2 seeded cases. Any `warning`/`error` finding on a clean control counts as a false positive. `info` findings are reported separately, not gated.

### 2.3 Growing the corpus
- **From prod:** when Hashim confirms (or rejects) a prod finding, a `promote-finding` script turns that PR head into a case. Real misses found later by humans are the most valuable, so add them as must_find.
- **Mutation seeding:** hand-written mutations of known-good code: drop a lock, swap `ctx` for `context.Background()`, log a secret, ignore an error, off-by-one in pagination.
- **Versioning:** a dataset manifest lists case IDs, and any change bumps `seeded-bugs-vN`. Baselines are tied to a dataset version, so they are never compared across versions.
- Target sizes: v1 has ~13 bugs across ~8 cases + 4 controls. v2 has ~30 cases.

## 3. Eval mode (real reviewer path, no posting)

- **Same code path.** `EvalRunWorkflow` starts the existing `CodeReviewWorkflow` as a **child workflow**, with `ReviewRequest{Diff: <head.patch>, Model, LabelKey: "eval", RunKind: "eval", Eval: {Dataset, CaseID, RunID, Experiment}}`. `RunReviewAgent` and `cursor-reviewer` are unchanged, so the prompt and CLI flags are identical to prod.
- **Prompt version** is not a code fork. A run can only evaluate the prompt template baked into the deployed image. Its hash (`agentflow.prompt.template_hash`, span design §11.2) is recorded and becomes the experiment key. To compare prompt versions A and B, run the same dataset on two image tags, or on two worker deployments during a transition.
- **No PR posting.** When `RunKind=eval`, `PostReview` writes only the `agentflow.review_record/v1` artifact (span design §11.6) and returns. It never calls GitHub or Origin. As a guardrail, `PostReview` refuses a GitHub/Origin sink when `RunKind=eval` (non-retryable `EvalSinkViolation`).
- **IDs.** The workflow ID is `eval:<experiment>:<case_id>:<model>:r<repeat>`. It never collides with `review:*`, so prod dedupe can't see it. Every child gets a fresh trace.
- **Isolation from prod.**
  - Eval workflows and activities run on a dedicated task queue, **`code-review-eval`**, served by a separate worker Deployment (the same image, `worker.maxConcurrentActivities: 1–2`).
  - Prod keeps `code-review` with its 4 slots. The PR #1 finding #11 (poller sharing slots) already argues against piling more onto that queue.
  - The `pr-poller` schedule never targets `code-review-eval`.
  - Optionally scale the eval worker to 0 outside runs.
- **Rate and usage budget.**
  - Each model gets a per-experiment concurrency cap (default 1 for Opus), and the run stops early (fail-fast) after N consecutive `usage_limited` outcomes.
  - **Opus is currently usage-limited until Nov 15.** Until then, either run grok-4.7 only, or let Opus cases record `usage_limited`.
- **Usage limits are recorded, not scored.** A case×model run with outcome `usage_limited`, `failed`, `timed_out` or `undetermined` is excluded from recall, precision and severity metrics. It counts toward **outcome rates** (§6.3). An experiment where a model has more than X% non-scored outcomes (default 20%) is marked `incomplete` for that model and is never used as a baseline or gated.

## 4. EvalRunWorkflow design

```
EvalRunWorkflow(input EvalRunInput) → EvalResultRef
  1. LoadDataset activity         → cases[] (+ dataset version, content hashes)
  2. ResolveVersions activity     → prompt_template_hash, reviewer/cli/image versions, config_hash
  3. for case × model × repeat (bounded by per-model concurrency):
        ExecuteChildWorkflow(CodeReviewWorkflow, eval request)  on task queue code-review-eval
        → child result ReviewResult; record artifact path
  4. CollectRecords activity      → review_record/v1[] read from the sink (the source of truth, not traces)
  5. ScoreRun activity            → eval_result/v1 per (case, model, repeat)  (pure Go, deterministic)
  6. Summarize activity           → eval_experiment/v1 + summary.md; compare to baseline; gate verdict
  7. EmitScoreSpans activity      → agentflow.eval.score spans + gen_ai.evaluation.result events (Honeycomb)
  8. Report activity (optional)   → PR comment, only if reporting.enabled (§8)
```

`EvalRunInput`
```json
{ "dataset": "seeded-bugs-v1", "case_filter": ["*"], "models": ["grok-4.7", "claude-opus-5-5-high"],
  "prompt_version": "v3", "repeats": 3, "experiment": "2026-11-03-prompt-v3",
  "trigger": {"kind": "manual|schedule|change", "ref": "main@<sha>", "pr": 0},
  "baseline": "latest|<experiment id>|none", "max_concurrency": {"claude-opus-5-5-high": 1, "grok-4.7": 2},
  "fail_fast_usage_limited": 3 }
```

- Determinism: all I/O happens in activities, and the scoring code is pure, so `ScoreRun` can be re-run offline over stored records (`evalctl rescore <experiment>`).
- Use `continue-as-new` if history grows (cases × models × repeats > ~200).
- Child workflow failures are caught and turned into records with outcome `failed`/`undetermined`, so the run always completes and summarizes.

`agentflow.eval_result/v1` (one per case × model × repeat)
```json
{ "schema": "agentflow.eval_result/v1",
  "experiment": "2026-11-03-prompt-v3", "eval_run_id": "…", "dataset": "seeded-bugs-v1@3",
  "case_id": "pr1-full-1acf4bb", "case_kind": "full_pr", "model": "grok-4.7", "repeat": 1,
  "review_record": "s3://agentflow-evals/records/<eval_run_id>/<case>/<model>/r1.json",
  "trace_id": "…", "workflow_id": "eval:…", "versions": { "prompt_template_hash": "…", "cli": "2026.10.22", "image": "0.2.4", "config_hash": "…" },
  "outcome": "reviewed|reviewed_clean|failed|usage_limited|timed_out|undetermined|skipped",
  "scored": true,
  "matches": [ {"bug_id": "B1", "finding_id": "f_ab12…", "match": "exact|alias|window", "severity_ok": true, "severity_delta": 0} ],
  "missed": [ {"bug_id": "B7", "priority": "nice_to_find"} ],
  "false_positives": [ {"finding_id": "f_cd34…", "file": "…", "line": 12, "severity": "warning", "category": "other"} ],
  "allowed_extra": [ "f_ef56…" ],
  "metrics": { "must_recall": 0.83, "nice_recall": 0.40, "precision": 0.71, "fp_count": 2, "fp_warning_plus": 1,
               "severity_accuracy": 0.8, "wall_ms": 151000, "cli_duration_ms": 143000, "findings": 9 } }
```

`agentflow.eval_experiment/v1` (one per experiment)
```json
{ "schema": "agentflow.eval_experiment/v1",
  "experiment": "2026-11-03-prompt-v3", "eval_run_id": "…", "dataset": "seeded-bugs-v1@3",
  "trigger": {"kind": "change", "ref": "main@abc123", "pr": 7}, "started_at": "…", "ended_at": "…",
  "versions": { "prompt_template_hash": "…", "image": "…", "cli": "…", "config_hash": "…" },
  "per_model": {
    "grok-4.7": { "runs": 36, "scored": 35, "complete": true,
      "outcome_rates": {"reviewed": 0.86, "reviewed_clean": 0.11, "failed": 0.03, "usage_limited": 0, "timed_out": 0, "undetermined": 0},
      "must_recall": {"mean": 0.78, "stdev": 0.06, "min": 0.67, "per_repeat": [0.75, 0.83, 0.75]},
      "precision": {"mean": 0.70, "stdev": 0.05}, "fp_per_clean_case": {"mean": 0.5},
      "severity_accuracy": {"mean": 0.81}, "latency_ms": {"p50": 148000, "p95": 402000},
      "per_bug_hit_rate": {"pr1-full/B1": 1.0, "pr1-full/B3": 0.67} },
    "claude-opus-5-5-high": { "complete": false, "reason": "usage_limited 100% (limit resets 2026-11-15)" } },
  "baseline": {"experiment": "2026-10-27-weekly", "deltas": {"grok-4.7": {"must_recall": -0.04, "precision": 0.02, "latency_p95_ms": 31000}}},
  "gate": {"status": "pass|fail|skipped", "violations": [ {"model": "grok-4.7", "metric": "must_recall", "baseline": 0.82, "value": 0.70, "threshold": "-0.10"} ] },
  "artifacts": {"results_prefix": "s3://agentflow-evals/results/<experiment>/", "summary_md": "…/summary.md"} }
```

## 5. Triggers

| Trigger | Mechanism | Default scope |
|---|---|---|
| Manual | `evalctl run --dataset seeded-bugs-v1 --models grok-4.7,claude-opus-5-5-high --repeats 3`, a thin wrapper over the Temporal client, or `temporal workflow start --type EvalRunWorkflow --task-queue code-review-eval --input @run.json` | anything |
| Weekly schedule | Temporal Schedule `eval-weekly` (e.g. Mon 06:00 America/Los_Angeles, overlap Skip), `baseline=latest` | all models, 3 repeats |
| Change-triggered | On each `pr-poller` run (or the webhook), compare `ResolveVersions` (prompt template hash, default models in label rules, reviewer config hash, CLI version) against the last recorded versions on `main`. If any differ, start `EvalRunWorkflow` with `trigger.kind=change`. A PR that changes `internal/reviewer/` or label rules can opt in via a label such as `agentflow-eval` (kept separate from `agentic-review-*`). | changed model(s) only, 3 repeats |

- Change detection uses the **prompt template hash** (span design §11.2), so a rename of variables or whitespace in the template counts as a change. That is intended.
- Debounce: at most one change-triggered run per (versions tuple) per 24 hours.

## 6. Scoring

### 6.1 Matching (deterministic, v1)
For each case run, take the record's findings (span design §11.3: `id`, `file`, `line`, `severity`, `category`, `message_hash`) and the expected bugs:
1. Normalize the path (strip `a/`/`b/`, `./`).
2. A finding **hits** bug B if `file` equals B.file (or an alias file) **and** `line` is within B.lines ± `window` (default 5; line 0 = unknown, so a match on file alone is a weak match, counted but flagged) **and** the category is compatible (equal, or in a small compatibility map such as `resource_leak ~ error_handling`, `security ~ correctness` for auth bugs). Until the verdict schema has `category`, fall back to file + line only and record `category_checked=false`.
3. Assignment is one-to-one, greedy by smallest line distance. Extra findings on an already-matched bug are **duplicates**, not FPs.
4. Unmatched findings are checked against `allowed_extra`. The rest are **false positives**.

### 6.2 LLM-judge fallback (later phase, off by default)
For unmatched (finding, bug) pairs in the same file, ask a fixed judge model "does finding X describe bug B?" with the bug description. The judge's verdict goes in `match: "judge"` with the judge model and version, and is reported **separately**. It never counts toward gated metrics until it has been validated against human labels.

### 6.3 Metrics
| Metric | Definition | Notes |
|---|---|---|
| `must_recall` | hit must_find bugs / total must_find bugs | primary gate metric |
| `nice_recall` | same for nice_to_find | reported, not gated |
| `precision` | matched findings / (matched + FP), counting warning+ only | |
| `fp_per_clean_case` | warning+ findings on clean controls / clean cases | gate metric |
| `severity_accuracy` | share of hits with severity ≥ `min_severity`. Also reports mean \|delta\| vs `expected_severity` | |
| outcome rates | share of runs per outcome (`reviewed`, `reviewed_clean`, `failed`, `usage_limited`, `timed_out`, `undetermined`) | usage_limited is **not** scored |
| latency | p50/p95 of record `timing.review_wall_ms` (and `cli_duration_ms`) on scored runs | cost proxy |
| per-bug hit rate | hits / repeats per bug | finds flaky bugs |

No cost metric: the CLI reports no tokens. Wall time and CLI `duration_ms` are the cost proxies.

### 6.4 Nondeterminism
- Run each case × model with `repeats` (default 3, 5 for a baseline). Report mean, stdev, min and per-repeat values, plus per-bug hit rate.
- Gates compare **means** but require the drop to exceed `max(threshold, 2 × pooled stdev)` before failing. This keeps a noisy model from tripping gates. With 3 repeats the result is only directional, which the summary states plainly.

## 7. Regression gates

- **Baseline:** the most recent `complete` experiment for the same dataset version and model, promoted explicitly with `evalctl baseline promote <experiment>`. Weekly runs on main auto-promote only if their own gate passes. Stored at `baselines/<dataset>/<model>.json` (a pointer to the experiment).
- **Default thresholds (per model):**
  - `must_recall` drop > 0.10 absolute → **fail**
  - any must_find bug with hit rate 1.0 in baseline → 0 now → **fail** ("lost bug")
  - `fp_per_clean_case` increase > 0.5 → **fail**
  - `precision` drop > 0.15 → warn
  - `severity_accuracy` drop > 0.15 → warn
  - `failed + timed_out + undetermined` rate > 10% → **fail** (reliability)
  - latency p95 increase > 50% → warn
  - `usage_limited` → the gate is **skipped** for that model (status `skipped`, reason recorded), never passed
- **Reporting a failing gate:**
  - Always: `summary.md` + `eval_experiment/v1` in storage; Honeycomb spans; worker log line.
  - **PR comment only when Hashim enables it** (`eval.reporting.prComment: false` by default). When on, it is one upserted comment on the PR that triggered the change, with marker `<!-- agentflow-eval -->`, holding the table of metrics vs baseline and the violations. This is separate from review markers, so it never affects review dedupe.
  - The gate is advisory: no status check blocks merges until Hashim decides otherwise.

## 8. Where results live

- **Artifacts (source of truth):** MinIO (already in the cluster), bucket `agentflow-evals`:
  - `records/<eval_run_id>/<case_id>/<model>/r<n>.json`: review_record/v1, written by `PostReview` in eval mode
  - `results/<experiment>/<case_id>/<model>/r<n>.json`: eval_result/v1
  - `results/<experiment>/experiment.json` + `summary.md`
  - `baselines/<dataset>/<model>.json`
  - Write-once objects (no overwrite). Fall back to a PVC (`/data/evals`, same layout) if MinIO isn't reachable from the eval worker. Credentials come from a Secret mounted only into the eval worker.
- **Honeycomb (projection):**
  - Eval child traces arrive with `agentflow.run_kind=eval` and `eval.*`.
  - `EmitScoreSpans` writes one `agentflow.eval.score` span per eval_result. It is linked to the child's trace ID (from the record) and carries numeric attributes (`eval.score.must_recall`, `eval.score.precision`, `eval.score.fp_count`, `eval.score.severity_accuracy`, `eval.wall_ms`, `eval.outcome`). It also carries `gen_ai.evaluation.result` events (`gen_ai.evaluation.name`, `gen_ai.evaluation.score.value`), which Honeycomb shows in Span Events and the Gen AI tab.
  - It also writes one `agentflow.eval.experiment` span with the gate verdict.
- **Example queries / board `AgentFlow evals`** (all `WHERE agentflow.run_kind = eval`):
  1. Recall by model per experiment: `AVG(eval.score.must_recall) WHERE name = agentflow.eval.score GROUP BY gen_ai.request.model, eval.experiment`.
  2. Precision and FP side by side: `AVG(eval.score.precision), SUM(eval.score.fp_count) GROUP BY gen_ai.request.model`.
  3. Outcome mix: `COUNT GROUP BY gen_ai.request.model, eval.outcome` (shows Opus `usage_limited` until Nov 15).
  4. Latency: `HEATMAP(eval.wall_ms), P50(eval.wall_ms), P95(eval.wall_ms) GROUP BY gen_ai.request.model`.
  5. Flaky bugs: `COUNT_DISTINCT(eval.run_id) WHERE eval.bug_hit = true GROUP BY eval.case_id, eval.bug_id, gen_ai.request.model`, using one span event per bug match.
  6. Prompt versions: `AVG(eval.score.must_recall) GROUP BY agentflow.prompt.template_hash, gen_ai.request.model`.
  7. Trigger: `COUNT WHERE name = agentflow.eval.experiment AND eval.gate.status = fail` → notify (once Hashim chooses a channel).

The numbers in Honeycomb are for exploration only. Gates and reports read MinIO.

## 9. Phased implementation plan (rough effort, one engineer)

| Phase | Work | Effort | Depends on |
|---|---|---|---|
| **0: prerequisites** | PR #1 fixes: outcome recorded **only after PostReview succeeds** (not at cycle start), outcome-aware dedupe, subprocess **env allowlist**, SIGTERM + WaitDelay cancel. Plus `review_record/v1` writer and the outcome union (span design §4/§11.6) | 2–3 d | PR #1 |
| **1: corpus v1** | `evals/` layout and schemas; `seed-from-comment` script; full-PR case from `b54cbdb..1acf4bb`; Hashim labels the 13 findings (category/priority); 4 clean controls | 1–2 d (+1 h Hashim) | — |
| **2: eval mode** | `RunKind`/`Eval` on `ReviewRequest`; eval sink guard in `PostReview`; MinIO record writer; `code-review-eval` task queue + eval worker Deployment (1–2 slots) | 1–2 d | 0 |
| **3: runner + scorer** | `EvalRunWorkflow` + activities; deterministic matcher; metrics; eval_result/experiment JSON; `evalctl run/rescore/baseline`; golden tests for the scorer | 3–4 d | 1, 2 |
| **4: gates + triggers** | baseline store; thresholds; `eval-weekly` schedule; change detection on versions tuple; summary.md; optional PR comment (off by default) | 1–2 d | 3 |
| **5: Honeycomb projection** | score/experiment spans + `gen_ai.evaluation.result` events; board + trigger | 1 d | 3, span design phase 1 |
| **6: corpus v2 + focused cases** | per-bug focused patches on post-fix code; mutation cases; promote-from-prod script | 2–3 d, ongoing | 0 merged |
| **7: optional** | LLM-judge fallback (reported separately); Langfuse as an extra collector exporter with an `eval.*` → `langfuse.experiment.*` mapping | 2 d | — |

First useful result: end of phase 3 (~1.5–2 weeks), with grok-4.7 only until Opus resets on Nov 15.

## 10. Open questions for Hashim

1. **Ground truth:** are all 13 PR #1 findings real bugs? Do you agree with the must_find vs nice_to_find split in §2.1 (6 must, 7 nice)?
2. **Sandbox repo:** OK to create a private `hash167/agentflow-eval-sandbox` for sandbox-mode cases, or keep v1 patch-only?
3. **Prompt versions:** is "one prompt per image tag" acceptable, or do you want prompt templates selectable at runtime (e.g. `CURSOR_REVIEW_PROMPT_VERSION`) so A/B can run in one deployment?
4. **Budget:** how many eval reviews per week are acceptable on Cursor usage (36 runs ≈ 1.5 h of CLI time per full weekly run at 3 repeats)? Should Opus be excluded from weekly runs by default?
5. **Gating:** advisory only, or should a failing gate eventually block merges to prompt/config files?
6. **Reporting:** enable the PR summary comment from day one, or keep it off and rely on summary.md + Honeycomb? Is there a notification channel for weekly failures?
7. **Storage:** MinIO bucket `agentflow-evals` with which retention? Is a PVC fallback needed?
8. **Verdict schema:** OK to add an optional `category` field to the reviewer's output JSON (a small prompt change, which itself triggers a baseline reset)?
9. **Eval worker:** separate Deployment (recommended) vs the same pod with a second worker on `code-review-eval`?
