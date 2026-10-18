package config

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
)

type StdioTransport struct {
	Command string            `json:"command"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

type HTTPTransport struct {
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
}

type Transport struct {
	Type    string            `json:"type"` // "stdio" or "http"
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

type Upstream struct {
	Name      string    `json:"name"`
	Transport Transport `json:"transport"`
}

type APIKey struct {
	Name string `json:"name"`
	Key  string `json:"key"`
	// Allow patterns: "*", "<upstream>:*", or "<upstream>:<tool>".
	Allow []string `json:"allow"`
}

type Config struct {
	Host     string     `json:"host"`
	Port     int        `json:"port"`
	AuditLog string     `json:"auditLog"`
	APIKeys  []APIKey   `json:"apiKeys"`
	Upstreams []Upstream `json:"upstreams"`
}

var (
	upstreamNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	envVarRe       = regexp.MustCompile(`\$\{([A-Z0-9_]+)\}`)
)

// substituteEnv replaces ${VAR} references with environment values and fails
// loudly when a referenced variable is missing.
func substituteEnv(value, context string) (string, error) {
	var missing string
	result := envVarRe.ReplaceAllStringFunc(value, func(match string) string {
		name := envVarRe.FindStringSubmatch(match)[1]
		resolved, ok := os.LookupEnv(name)
		if !ok {
			missing = name
			return match
		}
		return resolved
	})
	if missing != "" {
		return "", fmt.Errorf("config references environment variable %s (in %s) but it is not set", missing, context)
	}
	return result, nil
}

func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read config file %s: %w", path, err)
	}

	cfg := &Config{Host: "127.0.0.1", Port: 4817, AuditLog: "./audit.jsonl"}
	if err := json.Unmarshal(raw, cfg); err != nil {
		return nil, fmt.Errorf("invalid JSON in %s: %w", path, err)
	}

	if len(cfg.APIKeys) == 0 {
		return nil, fmt.Errorf("config must define at least one API key")
	}
	if len(cfg.Upstreams) == 0 {
		return nil, fmt.Errorf("config must define at least one upstream")
	}

	seenKeys := map[string]bool{}
	for i := range cfg.APIKeys {
		apiKey := &cfg.APIKeys[i]
		if apiKey.Key, err = substituteEnv(apiKey.Key, "apiKeys."+apiKey.Name+".key"); err != nil {
			return nil, err
		}
		if len(apiKey.Key) < 8 {
			return nil, fmt.Errorf("API key %q must be at least 8 characters", apiKey.Name)
		}
		if len(apiKey.Allow) == 0 {
			return nil, fmt.Errorf("API key %q must have at least one allow pattern", apiKey.Name)
		}
		if seenKeys[apiKey.Key] {
			return nil, fmt.Errorf("duplicate API key value for %q", apiKey.Name)
		}
		seenKeys[apiKey.Key] = true
	}

	for i := range cfg.Upstreams {
		upstream := &cfg.Upstreams[i]
		if !upstreamNameRe.MatchString(upstream.Name) {
			return nil, fmt.Errorf("upstream name %q must be lowercase alphanumeric with dashes", upstream.Name)
		}
		switch upstream.Transport.Type {
		case "stdio":
			if upstream.Transport.Command == "" {
				return nil, fmt.Errorf("stdio upstream %q needs a command", upstream.Name)
			}
			for name, value := range upstream.Transport.Env {
				if upstream.Transport.Env[name], err = substituteEnv(value, "upstreams."+upstream.Name+".env."+name); err != nil {
					return nil, err
				}
			}
		case "http":
			if upstream.Transport.URL == "" {
				return nil, fmt.Errorf("http upstream %q needs a url", upstream.Name)
			}
			for name, value := range upstream.Transport.Headers {
				if upstream.Transport.Headers[name], err = substituteEnv(value, "upstreams."+upstream.Name+".headers."+name); err != nil {
					return nil, err
				}
			}
		default:
			return nil, fmt.Errorf("upstream %q has unknown transport type %q (want stdio or http)", upstream.Name, upstream.Transport.Type)
		}
	}

	return cfg, nil
}
