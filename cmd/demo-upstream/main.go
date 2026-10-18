// A small stdio MCP server used as the default upstream so the gateway runs
// end-to-end out of the box. Replace it with real upstreams in gateway.config.json.
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type textInput struct {
	Text string `json:"text" jsonschema:"the text to process"`
}

func textResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}

func uuidV4() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func main() {
	server := mcp.NewServer(&mcp.Implementation{Name: "demo-tools", Version: "0.1.0"}, nil)

	mcp.AddTool(server,
		&mcp.Tool{Name: "echo", Description: "Echoes the given text back. Useful for verifying gateway connectivity."},
		func(ctx context.Context, req *mcp.CallToolRequest, in textInput) (*mcp.CallToolResult, any, error) {
			return textResult(in.Text), nil, nil
		},
	)

	mcp.AddTool(server,
		&mcp.Tool{Name: "current_time", Description: "Returns the current server time as an ISO 8601 timestamp."},
		func(ctx context.Context, req *mcp.CallToolRequest, in any) (*mcp.CallToolResult, any, error) {
			return textResult(time.Now().UTC().Format(time.RFC3339)), nil, nil
		},
	)

	mcp.AddTool(server,
		&mcp.Tool{Name: "sha256", Description: "Computes the SHA-256 hex digest of the given text."},
		func(ctx context.Context, req *mcp.CallToolRequest, in textInput) (*mcp.CallToolResult, any, error) {
			digest := sha256.Sum256([]byte(in.Text))
			return textResult(hex.EncodeToString(digest[:])), nil, nil
		},
	)

	mcp.AddTool(server,
		&mcp.Tool{Name: "generate_uuid", Description: "Generates a random UUID v4."},
		func(ctx context.Context, req *mcp.CallToolRequest, in any) (*mcp.CallToolResult, any, error) {
			return textResult(uuidV4()), nil, nil
		},
	)

	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Fatal(err)
	}
}
