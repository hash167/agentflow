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

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func main() {
	configPath := "./orchestrator.config.json"
	if len(os.Args) > 1 {
		configPath = os.Args[1]
	} else if env := os.Getenv("ORCHESTRATOR_CONFIG"); env != "" {
		configPath = env
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

	activities := &orchestrator.Activities{Review: cfg.Review}
	reviewWorker := worker.New(temporalClient, cfg.Temporal.TaskQueue, worker.Options{})
	reviewWorker.RegisterWorkflow(orchestrator.CodeReviewWorkflow)
	reviewWorker.RegisterActivity(activities)

	workerErr := make(chan error, 1)
	go func() { workerErr <- reviewWorker.Run(worker.InterruptCh()) }()

	webhookToken := os.Getenv("WEBHOOK_TOKEN")

	mux := http.NewServeMux()
	mux.HandleFunc("POST /hooks/pr", func(w http.ResponseWriter, r *http.Request) {
		if webhookToken != "" && r.Header.Get("X-Webhook-Token") != webhookToken {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid or missing X-Webhook-Token"})
			return
		}
		var request orchestrator.ReviewRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20)).Decode(&request); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
			return
		}
		if request.Repo == "" || request.PRNumber <= 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "repo and prNumber are required"})
			return
		}
		if request.Diff == "" && request.DiffURL == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "one of diff or diffUrl is required"})
			return
		}

		workflowID := orchestrator.WorkflowID(request.Repo, request.PRNumber)
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		run, err := temporalClient.SignalWithStartWorkflow(
			ctx,
			workflowID,
			orchestrator.SignalNewPush,
			request,
			client.StartWorkflowOptions{ID: workflowID, TaskQueue: cfg.Temporal.TaskQueue},
			orchestrator.CodeReviewWorkflow,
			request,
		)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "cannot start workflow: " + err.Error()})
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]string{"workflowId": workflowID, "runId": run.GetRunID()})
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
		response, err := temporalClient.QueryWorkflow(ctx, orchestrator.WorkflowID(repo, pr), "", orchestrator.QueryStatus)
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
