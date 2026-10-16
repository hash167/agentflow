import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import {
  StdioClientTransport,
  getDefaultEnvironment,
} from "@modelcontextprotocol/sdk/client/stdio.js";
import { StreamableHTTPClientTransport } from "@modelcontextprotocol/sdk/client/streamableHttp.js";
import type { UpstreamConfig } from "./config.js";

export interface GatewayTool {
  upstream: string;
  name: string;
  description?: string;
  inputSchema: Record<string, unknown>;
}

interface UpstreamState {
  config: UpstreamConfig;
  client: Client | null;
  error: string | null;
}

export class UpstreamManager {
  private readonly upstreams = new Map<string, UpstreamState>();

  constructor(configs: UpstreamConfig[]) {
    for (const config of configs) {
      this.upstreams.set(config.name, { config, client: null, error: null });
    }
  }

  /** Connects every upstream; a failing upstream is recorded but does not stop the gateway. */
  async start(): Promise<void> {
    await Promise.all(
      [...this.upstreams.values()].map(async (state) => {
        try {
          state.client = await this.connect(state.config);
          state.error = null;
          console.log(`[upstream] connected: ${state.config.name}`);
        } catch (error) {
          state.error = error instanceof Error ? error.message : String(error);
          console.error(`[upstream] FAILED to connect ${state.config.name}: ${state.error}`);
        }
      }),
    );
  }

  private async connect(config: UpstreamConfig): Promise<Client> {
    const client = new Client({ name: "agent-gateway", version: "0.1.0" });
    if (config.transport.type === "stdio") {
      const transport = new StdioClientTransport({
        command: config.transport.command,
        args: config.transport.args,
        env: { ...getDefaultEnvironment(), ...config.transport.env },
      });
      await client.connect(transport);
    } else {
      const headers = config.transport.headers;
      const transport = new StreamableHTTPClientTransport(new URL(config.transport.url), {
        requestInit: Object.keys(headers).length > 0 ? { headers } : undefined,
      });
      await client.connect(transport);
    }
    return client;
  }

  status(): Array<{ name: string; connected: boolean; error: string | null }> {
    return [...this.upstreams.values()].map((state) => ({
      name: state.config.name,
      connected: state.client !== null,
      error: state.error,
    }));
  }

  async listAllTools(): Promise<GatewayTool[]> {
    const tools: GatewayTool[] = [];
    await Promise.all(
      [...this.upstreams.values()].map(async (state) => {
        if (!state.client) return;
        try {
          const result = await state.client.listTools();
          for (const tool of result.tools) {
            tools.push({
              upstream: state.config.name,
              name: tool.name,
              description: tool.description,
              inputSchema: tool.inputSchema as Record<string, unknown>,
            });
          }
        } catch (error) {
          console.error(`[upstream] listTools failed for ${state.config.name}:`, error);
        }
      }),
    );
    return tools.sort((a, b) =>
      `${a.upstream}/${a.name}`.localeCompare(`${b.upstream}/${b.name}`),
    );
  }

  async callTool(upstream: string, tool: string, args: Record<string, unknown> | undefined) {
    const state = this.upstreams.get(upstream);
    if (!state) throw new Error(`Unknown upstream "${upstream}"`);
    if (!state.client) throw new Error(`Upstream "${upstream}" is not connected (${state.error ?? "unknown error"})`);
    return state.client.callTool({ name: tool, arguments: args });
  }

  async stop(): Promise<void> {
    await Promise.all(
      [...this.upstreams.values()].map(async (state) => {
        if (state.client) await state.client.close().catch(() => {});
      }),
    );
  }
}
