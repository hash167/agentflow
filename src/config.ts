import { readFileSync } from "node:fs";
import { z } from "zod";

const upstreamName = z
  .string()
  .regex(/^[a-z0-9][a-z0-9-]*$/, "upstream names must be lowercase alphanumeric with dashes");

const stdioTransportSchema = z.object({
  type: z.literal("stdio"),
  command: z.string(),
  args: z.array(z.string()).default([]),
  env: z.record(z.string(), z.string()).default({}),
});

const httpTransportSchema = z.object({
  type: z.literal("http"),
  url: z.string().url(),
  headers: z.record(z.string(), z.string()).default({}),
});

const upstreamSchema = z.object({
  name: upstreamName,
  transport: z.discriminatedUnion("type", [stdioTransportSchema, httpTransportSchema]),
});

const apiKeySchema = z.object({
  name: z.string(),
  key: z.string().min(8, "API keys must be at least 8 characters"),
  // Allow patterns: "*" (all tools), "upstream:*" (all tools of one upstream),
  // or "upstream:tool_name" (a single tool).
  allow: z.array(z.string()).min(1),
});

const configSchema = z.object({
  host: z.string().default("127.0.0.1"),
  port: z.number().int().positive().default(4817),
  auditLog: z.string().default("./audit.jsonl"),
  apiKeys: z.array(apiKeySchema).min(1),
  upstreams: z.array(upstreamSchema).min(1),
});

export type GatewayConfig = z.infer<typeof configSchema>;
export type UpstreamConfig = z.infer<typeof upstreamSchema>;
export type ApiKeyConfig = z.infer<typeof apiKeySchema>;

/** Replaces ${VAR_NAME} with the value from the environment; throws when missing. */
function substituteEnv(value: string, context: string): string {
  return value.replace(/\$\{([A-Z0-9_]+)\}/g, (_, name: string) => {
    const resolved = process.env[name];
    if (resolved === undefined) {
      throw new Error(`Config references environment variable ${name} (in ${context}) but it is not set`);
    }
    return resolved;
  });
}

export function loadConfig(path: string): GatewayConfig {
  let raw: string;
  try {
    raw = readFileSync(path, "utf8");
  } catch {
    throw new Error(`Cannot read config file at ${path}`);
  }

  const parsed = configSchema.parse(JSON.parse(raw));

  for (const apiKey of parsed.apiKeys) {
    apiKey.key = substituteEnv(apiKey.key, `apiKeys.${apiKey.name}.key`);
  }
  for (const upstream of parsed.upstreams) {
    if (upstream.transport.type === "http") {
      for (const [header, value] of Object.entries(upstream.transport.headers)) {
        upstream.transport.headers[header] = substituteEnv(
          value,
          `upstreams.${upstream.name}.headers.${header}`,
        );
      }
    } else {
      for (const [name, value] of Object.entries(upstream.transport.env)) {
        upstream.transport.env[name] = substituteEnv(value, `upstreams.${upstream.name}.env.${name}`);
      }
    }
  }

  const keys = new Set<string>();
  for (const apiKey of parsed.apiKeys) {
    if (keys.has(apiKey.key)) {
      throw new Error(`Duplicate API key value for "${apiKey.name}"`);
    }
    keys.add(apiKey.key);
  }

  return parsed;
}
