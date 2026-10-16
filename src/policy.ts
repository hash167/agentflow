/**
 * Allow patterns, checked against an upstream name and the upstream's own tool name:
 *   "*"              -> every tool on every upstream
 *   "github:*"       -> every tool on the "github" upstream
 *   "github:get_pr"  -> exactly that tool
 */
export function isToolAllowed(patterns: string[], upstream: string, tool: string): boolean {
  for (const pattern of patterns) {
    if (pattern === "*") return true;
    const separator = pattern.indexOf(":");
    if (separator === -1) continue;
    const upstreamPattern = pattern.slice(0, separator);
    const toolPattern = pattern.slice(separator + 1);
    if (upstreamPattern !== upstream) continue;
    if (toolPattern === "*" || toolPattern === tool) return true;
  }
  return false;
}

const SEPARATOR = "__";

/** Gateway-facing tool name, e.g. "github__get_pr". */
export function toGatewayToolName(upstream: string, tool: string): string {
  return `${upstream}${SEPARATOR}${tool}`;
}

export function parseGatewayToolName(name: string): { upstream: string; tool: string } | null {
  const separator = name.indexOf(SEPARATOR);
  if (separator <= 0) return null;
  return { upstream: name.slice(0, separator), tool: name.slice(separator + SEPARATOR.length) };
}
