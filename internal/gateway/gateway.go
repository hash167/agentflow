// Package gateway wires authentication, per-principal MCP servers, policy
// enforcement, and audit logging into one http.Handler.
package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"agent-gateway/internal/audit"
	"agent-gateway/internal/config"
	"agent-gateway/internal/policy"
	"agent-gateway/internal/upstream"
)

type principalKey struct{}

func principalFrom(ctx context.Context) *config.APIKey {
	p, _ := ctx.Value(principalKey{}).(*config.APIKey)
	return p
}

func denialResult(message string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: message}},
	}
}

// buildMCPServer builds an MCP server scoped to one principal: only allowed
// tools are listed, and every call is policy-checked and audited.
func buildMCPServer(
	principal *config.APIKey,
	upstreams *upstream.Manager,
	auditLog *audit.Logger,
	ctx context.Context,
) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "agent-gateway", Version: "0.1.0"}, nil)

	// Audits denied calls and returns an LLM-visible error instead of a
	// protocol-level "unknown tool" error.
	server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method == "tools/call" {
				if params, ok := req.GetParams().(*mcp.CallToolParamsRaw); ok {
					up, tool, parsed := policy.ParseGatewayToolName(params.Name)
					if !parsed {
						return denialResult(fmt.Sprintf("Unknown tool %q", params.Name)), nil
					}
					if !policy.IsToolAllowed(principal.Allow, up, tool) {
						auditLog.Log(audit.Entry{
							Event:     audit.EventPolicyDenied,
							Principal: principal.Name,
							Upstream:  up,
							Tool:      tool,
						})
						return denialResult(fmt.Sprintf(
							"Tool %q is not allowed for API key %q", params.Name, principal.Name,
						)), nil
					}
				}
			}
			return next(ctx, method, req)
		}
	})

	for _, tool := range upstreams.Tools(ctx) {
		if !policy.IsToolAllowed(principal.Allow, tool.Upstream, tool.Name) {
			continue
		}
		description := "[via " + tool.Upstream + "]"
		if tool.Description != "" {
			description += " " + tool.Description
		}
		upstreamName, toolName := tool.Upstream, tool.Name
		server.AddTool(
			&mcp.Tool{
				Name:        policy.GatewayToolName(tool.Upstream, tool.Name),
				Description: description,
				InputSchema: tool.InputSchema,
			},
			func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				started := time.Now()
				result, err := upstreams.CallTool(ctx, upstreamName, toolName, req.Params.Arguments)
				durationMs := time.Since(started).Milliseconds()
				if err != nil {
					auditLog.Log(audit.Entry{
						Event:      audit.EventToolCall,
						Principal:  principal.Name,
						Upstream:   upstreamName,
						Tool:       toolName,
						OK:         audit.Bool(false),
						DurationMs: audit.Int64(durationMs),
						Error:      err.Error(),
					})
					return denialResult("Upstream error: " + err.Error()), nil
				}
				auditLog.Log(audit.Entry{
					Event:      audit.EventToolCall,
					Principal:  principal.Name,
					Upstream:   upstreamName,
					Tool:       toolName,
					OK:         audit.Bool(!result.IsError),
					DurationMs: audit.Int64(durationMs),
				})
				return result, nil
			},
		)
	}

	return server
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// NewHandler returns the gateway's root http.Handler.
func NewHandler(cfg *config.Config, upstreams *upstream.Manager, auditLog *audit.Logger) http.Handler {
	keysByValue := map[string]*config.APIKey{}
	for i := range cfg.APIKeys {
		keysByValue[cfg.APIKeys[i].Key] = &cfg.APIKeys[i]
	}

	mcpHandler := mcp.NewStreamableHTTPHandler(
		func(req *http.Request) *mcp.Server {
			principal := principalFrom(req.Context())
			return buildMCPServer(principal, upstreams, auditLog, req.Context())
		},
		&mcp.StreamableHTTPOptions{Stateless: true},
	)

	authenticated := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, hasBearer := bearerToken(r)
		principal := keysByValue[token]
		if !hasBearer || principal == nil {
			auditLog.Log(audit.Entry{Event: audit.EventAuthFailed})
			writeJSON(w, http.StatusUnauthorized, map[string]any{
				"jsonrpc": "2.0",
				"error":   map[string]any{"code": -32001, "message": "Unauthorized: provide Authorization: Bearer <api-key>"},
				"id":      nil,
			})
			return
		}
		ctx := context.WithValue(r.Context(), principalKey{}, principal)
		mcpHandler.ServeHTTP(w, r.WithContext(ctx))
	})

	mux := http.NewServeMux()
	mux.Handle("/mcp", authenticated)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "upstreams": upstreams.Status()})
	})
	mux.HandleFunc("/{$}", func(w http.ResponseWriter, _ *http.Request) {
		names := make([]string, 0, len(cfg.Upstreams))
		for _, u := range cfg.Upstreams {
			names = append(names, u.Name)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"service":     "agent-gateway",
			"mcpEndpoint": "/mcp",
			"auth":        "Authorization: Bearer <api-key>",
			"upstreams":   names,
		})
	})
	return mux
}

func bearerToken(r *http.Request) (string, bool) {
	const prefix = "Bearer "
	header := r.Header.Get("Authorization")
	if len(header) <= len(prefix) || header[:len(prefix)] != prefix {
		return "", false
	}
	return header[len(prefix):], true
}
