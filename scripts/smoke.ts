/**
 * End-to-end smoke test against a running gateway.
 * Usage: GATEWAY_URL=http://127.0.0.1:4817/mcp npm run smoke
 */
import { createHash } from "node:crypto";
import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { StreamableHTTPClientTransport } from "@modelcontextprotocol/sdk/client/streamableHttp.js";

const GATEWAY_URL = process.env.GATEWAY_URL ?? "http://127.0.0.1:4817/mcp";
const ADMIN_KEY = process.env.ADMIN_KEY ?? "dev-admin-key-change-me";
const REVIEWER_KEY = process.env.REVIEWER_KEY ?? "dev-reviewer-key-change-me";

let failures = 0;

function check(name: string, condition: boolean, detail?: string) {
  if (condition) {
    console.log(`  PASS  ${name}`);
  } else {
    failures += 1;
    console.error(`  FAIL  ${name}${detail ? ` — ${detail}` : ""}`);
  }
}

async function connect(apiKey: string): Promise<Client> {
  const client = new Client({ name: "smoke-test", version: "0.1.0" });
  const transport = new StreamableHTTPClientTransport(new URL(GATEWAY_URL), {
    requestInit: { headers: { Authorization: `Bearer ${apiKey}` } },
  });
  await client.connect(transport);
  return client;
}

function firstText(result: unknown): string {
  const content = (result as { content?: Array<{ type: string; text?: string }> }).content;
  return content?.find((item) => item.type === "text")?.text ?? "";
}

async function main() {
  console.log(`Smoke-testing gateway at ${GATEWAY_URL}\n`);

  console.log("1. Unauthenticated requests are rejected");
  const unauthenticated = await fetch(GATEWAY_URL, {
    method: "POST",
    headers: { "content-type": "application/json", accept: "application/json, text/event-stream" },
    body: JSON.stringify({ jsonrpc: "2.0", method: "tools/list", id: 1 }),
  });
  check("POST without key returns 401", unauthenticated.status === 401, `got ${unauthenticated.status}`);

  console.log("\n2. Admin key sees and can call every tool");
  const admin = await connect(ADMIN_KEY);
  const adminTools = (await admin.listTools()).tools.map((tool) => tool.name);
  check(
    "admin sees all 4 demo tools",
    ["demo-tools__echo", "demo-tools__current_time", "demo-tools__sha256", "demo-tools__generate_uuid"].every(
      (name) => adminTools.includes(name),
    ),
    `saw: ${adminTools.join(", ")}`,
  );
  const echo = await admin.callTool({ name: "demo-tools__echo", arguments: { text: "hello gateway" } });
  check("echo round-trips through the gateway", firstText(echo) === "hello gateway", firstText(echo));
  const hash = await admin.callTool({ name: "demo-tools__sha256", arguments: { text: "temporal" } });
  const expected = createHash("sha256").update("temporal").digest("hex");
  check("sha256 result matches local hash", firstText(hash) === expected, firstText(hash));
  await admin.close();

  console.log("\n3. Restricted key only sees its allowed tools");
  const reviewer = await connect(REVIEWER_KEY);
  const reviewerTools = (await reviewer.listTools()).tools.map((tool) => tool.name);
  check(
    "reviewer sees exactly current_time and sha256",
    reviewerTools.length === 2 &&
      reviewerTools.includes("demo-tools__current_time") &&
      reviewerTools.includes("demo-tools__sha256"),
    `saw: ${reviewerTools.join(", ")}`,
  );
  const time = await reviewer.callTool({ name: "demo-tools__current_time", arguments: {} });
  check("reviewer can call an allowed tool", !time.isError && firstText(time).length > 0, firstText(time));
  const denied = await reviewer.callTool({ name: "demo-tools__echo", arguments: { text: "nope" } });
  check(
    "reviewer is denied a disallowed tool",
    denied.isError === true && firstText(denied).includes("not allowed"),
    firstText(denied),
  );
  await reviewer.close();

  console.log(`\n${failures === 0 ? "All checks passed." : `${failures} check(s) FAILED.`}`);
  process.exit(failures === 0 ? 0 : 1);
}

main().catch((error) => {
  console.error("Smoke test crashed:", error);
  process.exit(1);
});
