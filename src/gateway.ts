import { Server } from "@modelcontextprotocol/sdk/server/index.js";
import { StreamableHTTPServerTransport } from "@modelcontextprotocol/sdk/server/streamableHttp.js";
import {
  CallToolRequestSchema,
  ListToolsRequestSchema,
} from "@modelcontextprotocol/sdk/types.js";
import express, { type Request, type Response } from "express";
import type { ApiKeyConfig, GatewayConfig } from "./config.js";
import { isToolAllowed, parseGatewayToolName, toGatewayToolName } from "./policy.js";
import type { AuditLogger } from "./audit.js";
import type { UpstreamManager } from "./upstreams.js";

/**
 * Builds a per-request MCP server scoped to one principal: the tool list and
 * every call are filtered through that principal's allow patterns.
 */
function buildMcpServer(
  principal: ApiKeyConfig,
  upstreams: UpstreamManager,
  audit: AuditLogger,
): Server {
  const server = new Server(
    { name: "agent-gateway", version: "0.1.0" },
    { capabilities: { tools: {} } },
  );

  server.setRequestHandler(ListToolsRequestSchema, async () => {
    const all = await upstreams.listAllTools();
    const visible = all.filter((tool) => isToolAllowed(principal.allow, tool.upstream, tool.name));
    return {
      tools: visible.map((tool) => ({
        name: toGatewayToolName(tool.upstream, tool.name),
        description: tool.description
          ? `[via ${tool.upstream}] ${tool.description}`
          : `[via ${tool.upstream}]`,
        inputSchema: tool.inputSchema,
      })),
    };
  });

  server.setRequestHandler(CallToolRequestSchema, async (request) => {
    const parsed = parseGatewayToolName(request.params.name);
    if (!parsed) {
      return {
        content: [{ type: "text", text: `Unknown tool "${request.params.name}"` }],
        isError: true,
      };
    }
    const { upstream, tool } = parsed;

    if (!isToolAllowed(principal.allow, upstream, tool)) {
      audit.log({ event: "policy_denied", principal: principal.name, upstream, tool });
      return {
        content: [
          {
            type: "text",
            text: `Tool "${request.params.name}" is not allowed for API key "${principal.name}"`,
          },
        ],
        isError: true,
      };
    }

    const started = Date.now();
    try {
      const result = await upstreams.callTool(
        upstream,
        tool,
        request.params.arguments as Record<string, unknown> | undefined,
      );
      audit.log({
        event: "tool_call",
        principal: principal.name,
        upstream,
        tool,
        ok: !result.isError,
        durationMs: Date.now() - started,
      });
      return result;
    } catch (error) {
      const message = error instanceof Error ? error.message : String(error);
      audit.log({
        event: "tool_call",
        principal: principal.name,
        upstream,
        tool,
        ok: false,
        durationMs: Date.now() - started,
        error: message,
      });
      return { content: [{ type: "text", text: `Upstream error: ${message}` }], isError: true };
    }
  });

  return server;
}

export function buildApp(config: GatewayConfig, upstreams: UpstreamManager, audit: AuditLogger) {
  const app = express();
  app.use(express.json({ limit: "4mb" }));

  const keysByValue = new Map(config.apiKeys.map((apiKey) => [apiKey.key, apiKey]));

  app.get("/healthz", (_req, res) => {
    res.json({ status: "ok", upstreams: upstreams.status() });
  });

  app.get("/", (_req, res) => {
    res.json({
      service: "agent-gateway",
      mcpEndpoint: "/mcp",
      auth: "Authorization: Bearer <api-key>",
      upstreams: upstreams.status().map((upstream) => upstream.name),
    });
  });

  const authenticate = (req: Request, res: Response): ApiKeyConfig | null => {
    const header = req.headers.authorization;
    const token = header?.startsWith("Bearer ") ? header.slice("Bearer ".length) : undefined;
    const principal = token ? keysByValue.get(token) : undefined;
    if (!principal) {
      audit.log({ event: "auth_failed", principal: null });
      res.status(401).json({
        jsonrpc: "2.0",
        error: { code: -32001, message: "Unauthorized: provide Authorization: Bearer <api-key>" },
        id: null,
      });
      return null;
    }
    return principal;
  };

  // Stateless mode: one MCP server + transport per POST, scoped to the caller's key.
  app.post("/mcp", async (req, res) => {
    const principal = authenticate(req, res);
    if (!principal) return;

    const server = buildMcpServer(principal, upstreams, audit);
    const transport = new StreamableHTTPServerTransport({ sessionIdGenerator: undefined });
    res.on("close", () => {
      void transport.close();
      void server.close();
    });

    try {
      await server.connect(transport);
      await transport.handleRequest(req, res, req.body);
    } catch (error) {
      console.error("[gateway] request failed:", error);
      if (!res.headersSent) {
        res.status(500).json({
          jsonrpc: "2.0",
          error: { code: -32603, message: "Internal server error" },
          id: null,
        });
      }
    }
  });

  const methodNotAllowed = (_req: Request, res: Response) => {
    res.status(405).json({
      jsonrpc: "2.0",
      error: { code: -32000, message: "Method not allowed: the gateway runs in stateless mode" },
      id: null,
    });
  };
  app.get("/mcp", methodNotAllowed);
  app.delete("/mcp", methodNotAllowed);

  return app;
}
