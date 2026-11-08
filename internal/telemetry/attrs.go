package telemetry

import (
	"strconv"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Span names (short, completed spans plus per-attempt activity children).
const (
	SpanPollerRun       = "agentflow.poller.run"
	SpanScanRepo        = "agentflow.scan.repo"
	SpanDedupeCheck     = "agentflow.dedupe.check"
	SpanStartTarget     = "agentflow.poller.start_target"
	SpanCycleStarted    = "agentflow.cycle.started"
	SpanCycleFinished   = "agentflow.cycle.finished"
	SpanReviewOutcome   = "agentflow.review.outcome"
	SpanDiffFetch       = "agentflow.diff.fetch"
	SpanInvokeAgent     = "invoke_agent agentflow-reviewer"
	SpanReviewerPrepare = "agentflow.reviewer.prepare"
	SpanReviewerSubproc = "agentflow.reviewer.subprocess"
	SpanReviewerParse   = "agentflow.reviewer.parse"
	SpanCommentUpsert   = "agentflow.comment.upsert"
	SpanAttemptStarted  = "agentflow.attempt.started"
	SpanAttemptFinished = "agentflow.attempt.finished"
	SpanCLIRun          = "agentflow.cli.run"
	SpanExecuteTool     = "execute_tool"
	SpanGatewayUpstream = "agentflow.gateway.upstream_call"
)

// Attribute keys. GenAI names follow OTel GenAI semconv; AgentFlow-specific
// fields use agentflow.*.
const (
	AttrRepo              = "agentflow.repo"
	AttrForge             = "agentflow.forge"
	AttrPRNumber          = "agentflow.pr.number"
	AttrHeadSHA           = "agentflow.pr.head_sha"
	AttrLabelKey          = "agentflow.label_key"
	AttrLabel             = "agentflow.label"
	AttrReviewKey         = "agentflow.review.key"
	AttrCycle             = "agentflow.cycle"
	AttrRunKind           = "agentflow.run_kind"
	AttrOutcome           = "agentflow.outcome"
	AttrSkipReason        = "agentflow.skip_reason"
	AttrErrorClass        = "agentflow.error.class"
	AttrUndetermined      = "agentflow.undetermined"
	AttrAttemptKind       = "agentflow.attempt.kind"
	AttrPreviousOutcome   = "agentflow.previous_attempt_outcome"
	AttrPollerSchedule    = "agentflow.poller.schedule_id"
	AttrPollerRepos       = "agentflow.poller.repos"
	AttrPollerOpenPRs     = "agentflow.poller.open_prs"
	AttrPollerUnlabeled   = "agentflow.poller.unlabeled"
	AttrPollerTargets     = "agentflow.poller.targets"
	AttrPollerTriggered   = "agentflow.poller.triggered"
	AttrPollerUnchanged   = "agentflow.poller.unchanged"
	AttrScanPRsOpen       = "agentflow.scan.prs_open"
	AttrScanPRsLabeled    = "agentflow.scan.prs_labeled"
	AttrScanSkipped       = "agentflow.scan.skipped_reason"
	AttrDedupeSource      = "agentflow.dedupe.source"
	AttrDedupeDecision    = "agentflow.dedupe.decision"
	AttrDedupePrevSHA     = "agentflow.dedupe.prev_sha"
	AttrDedupePrevModel   = "agentflow.dedupe.prev_model"
	AttrDiffBytes         = "agentflow.diff.bytes"
	AttrDiffFiles         = "agentflow.diff.files"
	AttrDiffSHA256        = "agentflow.diff.sha256"
	AttrDiffTruncated     = "agentflow.diff.truncated"
	AttrDiffOverBudget    = "agentflow.diff.over_budget"
	AttrHTTPStatus        = "http.response.status_code"
	AttrReviewerTimeoutS  = "agentflow.reviewer.timeout_s"
	AttrReviewerGateway   = "agentflow.reviewer.gateway_enabled"
	AttrReviewerMode      = "agentflow.reviewer.mode"
	AttrReviewerVersion   = "agentflow.reviewer.version"
	AttrReviewerExitClass = "agentflow.reviewer.exit_class"
	AttrReviewerStderr    = "agentflow.reviewer.stderr_tail"
	AttrPromptHash        = "agentflow.prompt.template_hash"
	AttrPromptVersion     = "agentflow.prompt.template_version"
	AttrConfigHash        = "agentflow.config.hash"
	AttrParseStage        = "agentflow.parse.stage"
	AttrParseCandidates   = "agentflow.parse.candidates"
	AttrFindingsCount     = "agentflow.findings.count"
	AttrSink              = "agentflow.sink"
	AttrCommentAction     = "agentflow.comment.action"
	AttrCommentMarker     = "agentflow.comment.marker"
	AttrCommentID         = "agentflow.comment.id"
	AttrCLIVersion        = "agentflow.cli.version"
	AttrCLISession        = "agentflow.cli.session_id"
	AttrCLIRequest        = "agentflow.cli.request_id"
	AttrCLIDurationMS     = "agentflow.cli.duration_ms"
	AttrCLIDurationAPIMS  = "agentflow.cli.duration_api_ms"
	AttrCLIIsError        = "agentflow.cli.is_error"
	AttrCLIOutputFormat   = "agentflow.cli.output_format"
	AttrGatewayUpstream   = "agentflow.gateway.upstream"
	AttrGatewayPrincipal  = "agentflow.gateway.principal"
	AttrGatewayPolicy     = "agentflow.gateway.policy"
	AttrToolOK            = "agentflow.tool.ok"
	AttrToolDurationMS    = "agentflow.tool.duration_ms"
	AttrToolName          = "gen_ai.tool.name"
	AttrToolCallID        = "gen_ai.tool.call.id"
	AttrGenAIOp           = "gen_ai.operation.name"
	AttrGenAIAgent        = "gen_ai.agent.name"
	AttrGenAIProvider     = "gen_ai.provider.name"
	AttrGenAIModel        = "gen_ai.request.model"
	AttrGenAIConv         = "gen_ai.conversation.id"
	AttrTemporalAttempt   = "temporal.activity.attempt"
	AttrTemporalActType   = "temporal.activity.type"
	AttrTemporalWorkflow  = "temporal.workflow.id"
	AttrTemporalRunID     = "temporal.run.id"
	AttrProcessPID        = "process.pid"
	AttrProcessExit       = "process.exit.code"
	AttrErrorType         = "error.type"
)

const (
	RunKindProd = "prod"
	RunKindEval = "eval"

	ForgeGitHub = "github"
	ForgeOrigin = "origin"

	AgentName      = "agentflow-reviewer"
	ProviderCursor = "cursor"

	OutcomeReviewed      = "reviewed"
	OutcomeReviewedClean = "reviewed_clean"
	OutcomeSkipped       = "skipped"
	OutcomeSuperseded    = "superseded"
	OutcomeFailed        = "failed"
	OutcomeUsageLimited  = "usage_limited"
	OutcomeTimedOut      = "timed_out"
	OutcomeUndetermined  = "undetermined"
)

// Correlation is the low-cardinality set copied onto every span in a cycle.
type Correlation struct {
	Repo      string
	Forge     string
	PRNumber  int
	HeadSHA   string
	LabelKey  string
	Label     string
	Model     string
	ReviewKey string
	RunKind   string
	Cycle     int
	Workflow  string
	RunID     string
}

func (c Correlation) Attrs() []attribute.KeyValue {
	forge := c.Forge
	if forge == "" {
		forge = ForgeGitHub
	}
	runKind := c.RunKind
	if runKind == "" {
		runKind = RunKindProd
	}
	out := []attribute.KeyValue{
		attribute.String(AttrForge, forge),
		attribute.String(AttrRunKind, runKind),
		attribute.String(AttrGenAIAgent, AgentName),
	}
	if c.Repo != "" {
		out = append(out, attribute.String(AttrRepo, c.Repo))
	}
	if c.PRNumber > 0 {
		out = append(out, attribute.Int(AttrPRNumber, c.PRNumber))
	}
	if c.HeadSHA != "" {
		out = append(out, attribute.String(AttrHeadSHA, c.HeadSHA))
	}
	if c.LabelKey != "" {
		out = append(out, attribute.String(AttrLabelKey, c.LabelKey))
	}
	if c.Label != "" {
		out = append(out, attribute.String(AttrLabel, c.Label))
	}
	if c.Model != "" {
		out = append(out, attribute.String(AttrGenAIModel, c.Model))
	}
	if c.ReviewKey != "" {
		out = append(out, attribute.String(AttrReviewKey, c.ReviewKey), attribute.String(AttrGenAIConv, c.ReviewKey))
	}
	if c.Cycle > 0 {
		out = append(out, attribute.Int(AttrCycle, c.Cycle))
	}
	if c.Workflow != "" {
		out = append(out, attribute.String(AttrTemporalWorkflow, c.Workflow))
	}
	if c.RunID != "" {
		out = append(out, attribute.String(AttrTemporalRunID, c.RunID))
	}
	return out
}

func ReviewKey(repo string, pr int, labelKey string) string {
	return "review:" + repo + "#" + strconv.Itoa(pr) + ":" + labelKey
}

func SetOutcome(span trace.Span, outcome, errClass, skipReason string) {
	if span == nil {
		return
	}
	span.SetAttributes(attribute.String(AttrOutcome, outcome))
	if skipReason != "" {
		span.SetAttributes(attribute.String(AttrSkipReason, Truncate(Redact(skipReason), 256)))
	}
	if errClass != "" {
		span.SetAttributes(attribute.String(AttrErrorClass, errClass), attribute.String(AttrErrorType, errClass))
	}
	switch outcome {
	case OutcomeFailed, OutcomeUsageLimited, OutcomeTimedOut:
		span.SetStatus(codes.Error, outcome)
	case OutcomeUndetermined:
		span.SetAttributes(attribute.Bool(AttrUndetermined, true))
		span.SetStatus(codes.Unset, outcome)
	default:
		span.SetStatus(codes.Ok, outcome)
	}
}
