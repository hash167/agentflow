// The code-review control plane: runs the Temporal worker that hosts
// CodeReviewWorkflow and an HTTP webhook receiver that starts/signals
// per-PR workflows.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"

	"agent-gateway/internal/orchestrator"
)

func runOperator(cfg *orchestrator.Config, c client.Client, args orchestrator.Args) int {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var result orchestrator.OperatorResult
	switch args.Command {
	case "trigger-poller":
		if !cfg.Poller.Enabled {
			result = orchestrator.OperatorResult{Status: orchestrator.StatusError, ScheduleID: cfg.Poller.ScheduleID, Detail: "poller is disabled"}
			break
		}
		handle := c.ScheduleClient().GetHandle(ctx, cfg.Poller.ScheduleID)
		result, _ = orchestrator.TriggerPollNow(ctx, handle, cfg.Poller.ScheduleID)
	case "retry-skipped":
		acts := &orchestrator.Activities{
			Review: cfg.Review, Poller: cfg.Poller, Temporal: c, TaskQueue: cfg.Temporal.TaskQueue,
		}
		result, _ = orchestrator.RunRetrySkipped(ctx, cfg, args.Repo, args.PR, args.HeadSHA, orchestrator.NewRetrySource(acts))
	default:
		result = orchestrator.OperatorResult{Status: orchestrator.StatusError, Detail: "unknown operator command"}
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(result)
	log.Printf("[orchestrator] %s %s", args.Command, result.Status)
	return result.ExitCode()
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func main() {
	parsed, err := orchestrator.ParseArgs(os.Args[1:])
	if err != nil {
		log.Fatalf("[orchestrator] %v", err)
	}
	configPath := parsed.ConfigPath
	if configPath == "" {
		if env := os.Getenv("ORCHESTRATOR_CONFIG"); env != "" {
			configPath = env
		} else {
			configPath = "./orchestrator.config.json"
		}
	}
	cfg, err := orchestrator.LoadConfig(configPath)
	if err != nil {
		log.Fatalf("[orchestrator] fatal: %v", err)
	}

	temporalClient, err := client.Dial(client.Options{
		HostPort:  cfg.Temporal.HostPort,
		Namespace: cfg.Temporal.Namespace,
	})
	if err != nil {
		log.Fatalf("[orchestrator] cannot reach Temporal at %s: %v (is the server running?)", cfg.Temporal.HostPort, err)
	}
	defer temporalClient.Close()

	if parsed.Command != "" {
		code := runOperator(cfg, temporalClient, parsed)
		temporalClient.Close()
		os.Exit(code)
	}

	_, reviewActivityTimeout, _ := cfg.Review.Timeouts() // validated in LoadConfig
	orchestrator.SetReviewActivityTimeout(reviewActivityTimeout)
	log.Printf("[orchestrator] review timeouts: reviewer=%s activity(StartToClose)=%s heartbeat=1m",
		cfg.Review.ReviewerTimeout, orchestrator.ReviewActivityTimeout())

	activities := &orchestrator.Activities{
		Review:    cfg.Review,
		Poller:    cfg.Poller,
		Temporal:  temporalClient,
		TaskQueue: cfg.Temporal.TaskQueue,
	}
	workerOptions := orchestrator.WorkerOptions(cfg.Worker)
	reviewWorker := worker.New(temporalClient, cfg.Temporal.TaskQueue, workerOptions)
	reviewWorker.RegisterWorkflow(orchestrator.CodeReviewWorkflow)
	reviewWorker.RegisterWorkflow(orchestrator.PRPollerWorkflow)
	reviewWorker.RegisterActivity(activities)
	log.Printf("[orchestrator] worker caps: maxConcurrentActivities=%d maxConcurrentWorkflowTasks=%d (0 = SDK default)",
		workerOptions.MaxConcurrentActivityExecutionSize, workerOptions.MaxConcurrentWorkflowTaskExecutionSize)
	log.Printf("[orchestrator] reviewer=%v sink=%s", cfg.Review.ReviewerCommand, cfg.Review.Sink.Type)
	for _, rule := range cfg.Review.Labels {
		log.Printf("[orchestrator] review label %q -> key=%s model=%s", rule.Label, rule.Key, rule.Model)
	}

	if cfg.Poller.Enabled {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		if err := orchestrator.EnsurePollerSchedule(ctx, temporalClient, cfg.Poller, cfg.Temporal.TaskQueue); err != nil {
			log.Fatalf("[orchestrator] cannot ensure poller schedule %s: %v", cfg.Poller.ScheduleID, err)
		}
		cancel()
		log.Printf("[orchestrator] poller schedule %s ensured: every %s, owner=%s, overlap=skip", cfg.Poller.ScheduleID, cfg.Poller.Interval, cfg.Poller.Owner)
	} else {
		log.Printf("[orchestrator] poller disabled (poller.enabled=false)")
	}

	workerErr := make(chan error, 1)
	go func() { workerErr <- reviewWorker.Run(worker.InterruptCh()) }()

	webhookToken := os.Getenv("WEBHOOK_TOKEN")

	mux := http.NewServeMux()
	mux.HandleFunc("POST /hooks/pr", func(w http.ResponseWriter, r *http.Request) {
		if webhookToken != "" && r.Header.Get("X-Webhook-Token") != webhookToken {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid or missing X-Webhook-Token"})
			return
		}
		// Same label gating as the poller: "labels" lists the PR's labels and
		// one review workflow starts per matching review label.
		var body struct {
			orchestrator.ReviewRequest
			Labels []string `json:"labels"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20)).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
			return
		}
		request := body.ReviewRequest
		if request.Repo == "" || request.PRNumber <= 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "repo and prNumber are required"})
			return
		}
		if request.Diff == "" && request.DiffURL == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "one of diff or diffUrl is required"})
			return
		}

		rules := orchestrator.MatchLabels(cfg.Review.Labels, body.Labels)
		if len(rules) == 0 {
			writeJSON(w, http.StatusOK, map[string]any{"skipped": "no review label", "workflowIds": []string{}})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		ids := []string{}
		for _, rule := range rules {
			req := request
			req.LabelKey, req.Label, req.Model = rule.Key, rule.Label, rule.Model
			workflowID := orchestrator.ReviewWorkflowID(req.Repo, req.PRNumber, rule.Key)
			if err := orchestrator.StartReview(ctx, temporalClient, cfg.Temporal.TaskQueue, workflowID, req); err != nil {
				writeJSON(w, http.StatusBadGateway, map[string]any{"error": "cannot start workflow: " + err.Error(), "workflowIds": ids})
				return
			}
			ids = append(ids, workflowID)
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"workflowIds": ids})
	})

	mux.HandleFunc("GET /reviews", func(w http.ResponseWriter, r *http.Request) {
		repo := r.URL.Query().Get("repo")
		pr, _ := strconv.Atoi(r.URL.Query().Get("pr"))
		if repo == "" || pr <= 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "repo and pr query parameters are required"})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		key := r.URL.Query().Get("label") // label key, e.g. "default" or "claude"
		if key == "" {
			key = "default"
		}
		response, err := temporalClient.QueryWorkflow(ctx, orchestrator.ReviewWorkflowID(repo, pr, key), "", orchestrator.QueryStatus)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		var status orchestrator.Status
		if err := response.Get(&status); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, status)
	})

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "temporal": cfg.Temporal.HostPort})
	})

	address := fmt.Sprintf("%s:%d", cfg.Webhook.Host, cfg.Webhook.Port)
	httpServer := &http.Server{Addr: address, Handler: mux}
	httpErr := make(chan error, 1)
	go func() { httpErr <- httpServer.ListenAndServe() }()

	log.Printf("[orchestrator] webhook listening on http://%s/hooks/pr", address)
	log.Printf("[orchestrator] temporal=%s namespace=%s taskQueue=%s", cfg.Temporal.HostPort, cfg.Temporal.Namespace, cfg.Temporal.TaskQueue)
	if webhookToken == "" {
		log.Printf("[orchestrator] WEBHOOK_TOKEN not set; webhook is unauthenticated (fine for localhost only)")
	}

	select {
	case err := <-workerErr:
		if err != nil {
			log.Fatalf("[orchestrator] worker stopped: %v", err)
		}
		_ = httpServer.Shutdown(context.Background())
	case err := <-httpErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("[orchestrator] http server stopped: %v", err)
		}
	}
}
