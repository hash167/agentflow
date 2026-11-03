package orchestrator

import (
	"encoding/json"
	"fmt"
	"os"

	"go.temporal.io/sdk/worker"
)

type TemporalConfig struct {
	HostPort  string `json:"hostPort"`
	Namespace string `json:"namespace"`
	TaskQueue string `json:"taskQueue"`
}

type WebhookConfig struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

type SinkConfig struct {
	// Type is "file" (write JSON under Dir) or "github" (post a PR comment).
	Type string `json:"type"`
	Dir  string `json:"dir,omitempty"`
	// APIBase defaults to https://api.github.com; the token comes from the
	// GITHUB_TOKEN environment variable at runtime.
	APIBase string `json:"apiBase,omitempty"`
}

type ReviewConfig struct {
	// ReviewerCommand receives {"repo","prNumber","title","diff"} JSON on
	// stdin and must print a ReviewResult-shaped JSON object ({"summary",
	// "findings"}) on stdout. Swap the bundled mock for the headless Cursor
	// CLI (or any agent) in production.
	ReviewerCommand []string   `json:"reviewerCommand"`
	MaxDiffBytes    int        `json:"maxDiffBytes"`
	Sink            SinkConfig `json:"sink"`
}

// WorkerConfig caps how much work this worker runs at once. Reviews beyond
// the cap stay queued in Temporal (activity tasks wait on the task queue; the
// 10m StartToClose clock only starts once a slot picks them up).
type WorkerConfig struct {
	// MaxConcurrentActivities bounds concurrently executing activities
	// (FetchDiff, RunReviewAgent, PostReview, poller activities). Default 4.
	MaxConcurrentActivities int `json:"maxConcurrentActivities"`
	// MaxConcurrentWorkflowTasks bounds concurrent workflow tasks; 0 keeps the
	// SDK default. Workflow tasks are cheap, so this rarely needs changing.
	MaxConcurrentWorkflowTasks int `json:"maxConcurrentWorkflowTasks"`
}

type Config struct {
	Temporal TemporalConfig `json:"temporal"`
	Webhook  WebhookConfig  `json:"webhook"`
	Review   ReviewConfig   `json:"review"`
	Worker   WorkerConfig   `json:"worker"`
	Poller   PollerConfig   `json:"poller"`
}

func LoadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read config file %s: %w", path, err)
	}
	cfg := &Config{
		Temporal: TemporalConfig{HostPort: "127.0.0.1:7233", Namespace: "default", TaskQueue: "code-review"},
		Webhook:  WebhookConfig{Host: "127.0.0.1", Port: 4819},
		Review:   ReviewConfig{MaxDiffBytes: 300_000, Sink: SinkConfig{Type: "file", Dir: "./reviews"}},
		Worker:   WorkerConfig{MaxConcurrentActivities: DefaultMaxConcurrentActivities},
		Poller:   PollerConfig{ScheduleID: "pr-poller", Interval: "15m", Owner: "hash167", IncludeDrafts: true},
	}
	if err := json.Unmarshal(raw, cfg); err != nil {
		return nil, fmt.Errorf("invalid JSON in %s: %w", path, err)
	}
	if len(cfg.Review.ReviewerCommand) == 0 {
		return nil, fmt.Errorf("review.reviewerCommand must be set")
	}
	switch cfg.Review.Sink.Type {
	case "file":
		if cfg.Review.Sink.Dir == "" {
			return nil, fmt.Errorf("review.sink.dir must be set for the file sink")
		}
	case "github":
	default:
		return nil, fmt.Errorf("review.sink.type must be \"file\" or \"github\", got %q", cfg.Review.Sink.Type)
	}
	if cfg.Worker.MaxConcurrentActivities < 0 || cfg.Worker.MaxConcurrentWorkflowTasks < 0 {
		return nil, fmt.Errorf("worker.maxConcurrentActivities and worker.maxConcurrentWorkflowTasks must be >= 0")
	}
	if cfg.Worker.MaxConcurrentActivities == 0 {
		cfg.Worker.MaxConcurrentActivities = DefaultMaxConcurrentActivities
	}
	if cfg.Poller.Enabled {
		if _, err := cfg.Poller.IntervalDuration(); err != nil {
			return nil, err
		}
		if cfg.Poller.Owner == "" {
			return nil, fmt.Errorf("poller.owner must be set when the poller is enabled")
		}
	}
	return cfg, nil
}

const DefaultMaxConcurrentActivities = 4

// WorkerOptions turns the worker config into Temporal worker options.
func WorkerOptions(cfg WorkerConfig) worker.Options {
	opts := worker.Options{
		MaxConcurrentActivityExecutionSize:     cfg.MaxConcurrentActivities,
		MaxConcurrentWorkflowTaskExecutionSize: cfg.MaxConcurrentWorkflowTasks,
	}
	if opts.MaxConcurrentActivityExecutionSize <= 0 {
		opts.MaxConcurrentActivityExecutionSize = DefaultMaxConcurrentActivities
	}
	return opts
}
