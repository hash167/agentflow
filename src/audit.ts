import { appendFileSync, mkdirSync } from "node:fs";
import { dirname, resolve } from "node:path";

export interface AuditEntry {
  ts: string;
  event: "tool_call" | "policy_denied" | "auth_failed";
  principal: string | null;
  tool?: string;
  upstream?: string;
  ok?: boolean;
  durationMs?: number;
  error?: string;
}

export class AuditLogger {
  private readonly path: string;

  constructor(path: string) {
    this.path = resolve(path);
    mkdirSync(dirname(this.path), { recursive: true });
  }

  log(entry: Omit<AuditEntry, "ts">): void {
    const full: AuditEntry = { ts: new Date().toISOString(), ...entry };
    appendFileSync(this.path, JSON.stringify(full) + "\n");
    const outcome = full.event === "tool_call" ? (full.ok ? "ok" : "error") : full.event;
    console.log(
      `[audit] ${full.event} principal=${full.principal ?? "-"} tool=${full.tool ?? "-"} ${outcome}` +
        (full.durationMs !== undefined ? ` ${full.durationMs}ms` : ""),
    );
  }
}
