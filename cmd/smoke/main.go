// End-to-end smoke test against a running gateway.
// Usage: GATEWAY_URL=http://127.0.0.1:4817/mcp go run ./cmd/smoke
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var (
	gatewayURL  = envOr("GATEWAY_URL", "http://127.0.0.1:4817/mcp")
	adminKey    = envOr("ADMIN_KEY", "dev-admin-key-change-me")
	reviewerKey = envOr("REVIEWER_KEY", "dev-reviewer-key-change-me")
	failures    = 0
)

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func check(name string, condition bool, detail string) {
	if condition {
		fmt.Printf("  PASS  %s\n", name)
	} else {
		failures++
		fmt.Printf("  FAIL  %s — %s\n", name, detail)
	}
}

type headerTransport struct {
	authorization string
}

func (t *headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.Header.Set("Authorization", t.authorization)
	return http.DefaultTransport.RoundTrip(req)
}

func connect(ctx context.Context, apiKey string) (*mcp.ClientSession, error) {
	client := mcp.NewClient(&mcp.Implementation{Name: "smoke-test", Version: "0.1.0"}, nil)
	return client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             gatewayURL,
		HTTPClient:           &http.Client{Transport: &headerTransport{authorization: "Bearer " + apiKey}},
		DisableStandaloneSSE: true,
	}, nil)
}

func firstText(result *mcp.CallToolResult) string {
	for _, content := range result.Content {
		if text, ok := content.(*mcp.TextContent); ok {
			return text.Text
		}
	}
	return ""
}

func toolNames(ctx context.Context, session *mcp.ClientSession) ([]string, error) {
	result, err := session.ListTools(ctx, nil)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(result.Tools))
	for _, tool := range result.Tools {
		names = append(names, tool.Name)
	}
	return names, nil
}

func main() {
	ctx := context.Background()
	fmt.Printf("Smoke-testing gateway at %s\n\n", gatewayURL)

	fmt.Println("1. Unauthenticated requests are rejected")
	body := []byte(`{"jsonrpc":"2.0","method":"tools/list","id":1}`)
	req, _ := http.NewRequest(http.MethodPost, gatewayURL, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Printf("cannot reach gateway: %v\n", err)
		os.Exit(1)
	}
	response.Body.Close()
	check("POST without key returns 401", response.StatusCode == http.StatusUnauthorized,
		fmt.Sprintf("got %d", response.StatusCode))

	fmt.Println("\n2. Admin key sees and can call every tool")
	admin, err := connect(ctx, adminKey)
	if err != nil {
		fmt.Printf("admin connect failed: %v\n", err)
		os.Exit(1)
	}
	adminTools, err := toolNames(ctx, admin)
	check("admin can list tools", err == nil, fmt.Sprint(err))
	expected := []string{"demo-tools__echo", "demo-tools__current_time", "demo-tools__sha256", "demo-tools__generate_uuid"}
	hasAll := true
	for _, name := range expected {
		if !slices.Contains(adminTools, name) {
			hasAll = false
		}
	}
	check("admin sees all 4 demo tools", hasAll, "saw: "+strings.Join(adminTools, ", "))

	echo, err := admin.CallTool(ctx, &mcp.CallToolParams{
		Name: "demo-tools__echo", Arguments: map[string]any{"text": "hello gateway"},
	})
	check("echo round-trips through the gateway",
		err == nil && firstText(echo) == "hello gateway", fmt.Sprintf("err=%v", err))

	hash, err := admin.CallTool(ctx, &mcp.CallToolParams{
		Name: "demo-tools__sha256", Arguments: map[string]any{"text": "temporal"},
	})
	digest := sha256.Sum256([]byte("temporal"))
	check("sha256 result matches local hash",
		err == nil && firstText(hash) == hex.EncodeToString(digest[:]), fmt.Sprintf("err=%v", err))
	admin.Close()

	fmt.Println("\n3. Restricted key only sees its allowed tools")
	reviewer, err := connect(ctx, reviewerKey)
	if err != nil {
		fmt.Printf("reviewer connect failed: %v\n", err)
		os.Exit(1)
	}
	reviewerTools, err := toolNames(ctx, reviewer)
	check("reviewer sees exactly current_time and sha256",
		err == nil && len(reviewerTools) == 2 &&
			slices.Contains(reviewerTools, "demo-tools__current_time") &&
			slices.Contains(reviewerTools, "demo-tools__sha256"),
		"saw: "+strings.Join(reviewerTools, ", "))

	allowed, err := reviewer.CallTool(ctx, &mcp.CallToolParams{Name: "demo-tools__current_time"})
	check("reviewer can call an allowed tool",
		err == nil && !allowed.IsError && firstText(allowed) != "", fmt.Sprintf("err=%v", err))

	denied, err := reviewer.CallTool(ctx, &mcp.CallToolParams{
		Name: "demo-tools__echo", Arguments: map[string]any{"text": "nope"},
	})
	check("reviewer is denied a disallowed tool",
		err == nil && denied.IsError && strings.Contains(firstText(denied), "not allowed"),
		fmt.Sprintf("err=%v text=%q", err, firstText(denied)))
	reviewer.Close()

	if failures == 0 {
		fmt.Println("\nAll checks passed.")
		os.Exit(0)
	}
	fmt.Printf("\n%d check(s) FAILED.\n", failures)
	os.Exit(1)
}
