package orchestrator

import (
	"encoding/json"
	"fmt"
	"os"
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
	ReviewerCommand []string `json:"reviewerCommand"`
	MaxDiffBytes    int      `json:"maxDiffBytes"`
	Sink            SinkConfig `json:"sink"`
}

type Config struct {
	Temporal TemporalConfig `json:"temporal"`
	Webhook  WebhookConfig  `json:"webhook"`
	Review   ReviewConfig   `json:"review"`
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
	return cfg, nil
}
