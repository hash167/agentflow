/**
 * A small stdio MCP server used as the default upstream so the gateway runs
 * end-to-end out of the box. Replace it with real upstreams in gateway.config.json.
 */
import { createHash, randomUUID } from "node:crypto";
import { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import { StdioServerTransport } from "@modelcontextprotocol/sdk/server/stdio.js";
import { z } from "zod";

const server = new McpServer({ name: "demo-tools", version: "0.1.0" });

server.registerTool(
  "echo",
  {
    description: "Echoes the given text back. Useful for verifying gateway connectivity.",
    inputSchema: { text: z.string().describe("Text to echo back") },
  },
  async ({ text }) => ({ content: [{ type: "text", text }] }),
);

server.registerTool(
  "current_time",
  {
    description: "Returns the current server time as an ISO 8601 timestamp.",
    inputSchema: {},
  },
  async () => ({ content: [{ type: "text", text: new Date().toISOString() }] }),
);

server.registerTool(
  "sha256",
  {
    description: "Computes the SHA-256 hex digest of the given text.",
    inputSchema: { text: z.string().describe("Text to hash") },
  },
  async ({ text }) => ({
    content: [{ type: "text", text: createHash("sha256").update(text).digest("hex") }],
  }),
);

server.registerTool(
  "generate_uuid",
  {
    description: "Generates a random UUID v4.",
    inputSchema: {},
  },
  async () => ({ content: [{ type: "text", text: randomUUID() }] }),
);

const transport = new StdioServerTransport();
await server.connect(transport);
