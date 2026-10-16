import { loadConfig } from "./config.js";
import { AuditLogger } from "./audit.js";
import { UpstreamManager } from "./upstreams.js";
import { buildApp } from "./gateway.js";

async function main() {
  const configPath = process.argv[2] ?? process.env.GATEWAY_CONFIG ?? "./gateway.config.json";
  const config = loadConfig(configPath);

  const audit = new AuditLogger(config.auditLog);
  const upstreams = new UpstreamManager(config.upstreams);
  await upstreams.start();

  const app = buildApp(config, upstreams, audit);
  const server = app.listen(config.port, config.host, () => {
    console.log(`[gateway] listening on http://${config.host}:${config.port}/mcp`);
    console.log(`[gateway] ${config.apiKeys.length} API key(s), ${config.upstreams.length} upstream(s)`);
  });

  const shutdown = async () => {
    console.log("[gateway] shutting down");
    server.close();
    await upstreams.stop();
    process.exit(0);
  };
  process.on("SIGINT", shutdown);
  process.on("SIGTERM", shutdown);
}

main().catch((error) => {
  console.error("[gateway] fatal:", error instanceof Error ? error.message : error);
  process.exit(1);
});
