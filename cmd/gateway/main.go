package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"agent-gateway/internal/audit"
	"agent-gateway/internal/config"
	"agent-gateway/internal/gateway"
	"agent-gateway/internal/upstream"
)

func main() {
	configPath := "./gateway.config.json"
	if len(os.Args) > 1 {
		configPath = os.Args[1]
	} else if env := os.Getenv("GATEWAY_CONFIG"); env != "" {
		configPath = env
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		log.Fatalf("[gateway] fatal: %v", err)
	}

	auditLog, err := audit.New(cfg.AuditLog)
	if err != nil {
		log.Fatalf("[gateway] fatal: %v", err)
	}
	defer auditLog.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	upstreams := upstream.NewManager(cfg.Upstreams)
	upstreams.Start(ctx)
	defer upstreams.Stop()

	address := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	server := &http.Server{Addr: address, Handler: gateway.NewHandler(cfg, upstreams, auditLog)}

	go func() {
		<-ctx.Done()
		log.Println("[gateway] shutting down")
		_ = server.Shutdown(context.Background())
	}()

	log.Printf("[gateway] listening on http://%s/mcp", address)
	log.Printf("[gateway] %d API key(s), %d upstream(s)", len(cfg.APIKeys), len(cfg.Upstreams))
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("[gateway] fatal: %v", err)
	}
}
