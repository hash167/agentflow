// Package policy implements per-key tool allowlists and gateway tool naming.
//
// Allow patterns, checked against an upstream name and the upstream's own
// tool name:
//
//	"*"              -> every tool on every upstream
//	"github:*"       -> every tool on the "github" upstream
//	"github:get_pr"  -> exactly that tool
package policy

import "strings"

// Separator joins upstream and tool into the gateway-facing tool name,
// e.g. "github__get_pr".
const Separator = "__"

func IsToolAllowed(patterns []string, upstream, tool string) bool {
	for _, pattern := range patterns {
		if pattern == "*" {
			return true
		}
		upstreamPattern, toolPattern, found := strings.Cut(pattern, ":")
		if !found || upstreamPattern != upstream {
			continue
		}
		if toolPattern == "*" || toolPattern == tool {
			return true
		}
	}
	return false
}

func GatewayToolName(upstream, tool string) string {
	return upstream + Separator + tool
}

// ParseGatewayToolName splits a gateway tool name back into upstream and tool;
// ok is false when the name is not namespaced.
func ParseGatewayToolName(name string) (upstream, tool string, ok bool) {
	upstream, tool, found := strings.Cut(name, Separator)
	if !found || upstream == "" || tool == "" {
		return "", "", false
	}
	return upstream, tool, true
}
