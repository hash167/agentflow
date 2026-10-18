// Package upstream manages MCP client connections to the configured upstream
// servers and exposes an aggregated, cached view of their tools.
package upstream

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"agent-gateway/internal/config"
)

const toolCacheTTL = 15 * time.Second

// Tool is one upstream tool as seen by the gateway (pre-namespacing).
type Tool struct {
	Upstream    string
	Name        string
	Description string
	InputSchema any
}

type Status struct {
	Name      string `json:"name"`
	Connected bool   `json:"connected"`
	Error     string `json:"error,omitempty"`
}

type state struct {
	config  config.Upstream
	session *mcp.ClientSession
	err     string
}

type Manager struct {
	mu        sync.Mutex
	upstreams map[string]*state
	order     []string

	toolsCache   []Tool
	toolsFetched time.Time
}

func NewManager(upstreams []config.Upstream) *Manager {
	m := &Manager{upstreams: map[string]*state{}}
	for _, u := range upstreams {
		m.upstreams[u.Name] = &state{config: u}
		m.order = append(m.order, u.Name)
	}
	return m
}

type headerTransport struct {
	headers map[string]string
	base    http.RoundTripper
}

func (t *headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	for name, value := range t.headers {
		req.Header.Set(name, value)
	}
	return t.base.RoundTrip(req)
}

func connect(ctx context.Context, cfg config.Upstream) (*mcp.ClientSession, error) {
	client := mcp.NewClient(&mcp.Implementation{Name: "agent-gateway", Version: "0.1.0"}, nil)

	var transport mcp.Transport
	if cfg.Transport.Type == "stdio" {
		cmd := exec.Command(cfg.Transport.Command, cfg.Transport.Args...)
		cmd.Env = os.Environ()
		for name, value := range cfg.Transport.Env {
			cmd.Env = append(cmd.Env, name+"="+value)
		}
		cmd.Stderr = os.Stderr
		transport = &mcp.CommandTransport{Command: cmd}
	} else {
		httpClient := http.DefaultClient
		if len(cfg.Transport.Headers) > 0 {
			httpClient = &http.Client{
				Transport: &headerTransport{headers: cfg.Transport.Headers, base: http.DefaultTransport},
			}
		}
		transport = &mcp.StreamableClientTransport{
			Endpoint:   cfg.Transport.URL,
			HTTPClient: httpClient,
		}
	}

	return client.Connect(ctx, transport, nil)
}

// Start connects every upstream; a failing upstream is recorded but does not
// stop the gateway.
func (m *Manager) Start(ctx context.Context) {
	var wg sync.WaitGroup
	for _, name := range m.order {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			s := m.upstreams[name]
			connectCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			session, err := connect(connectCtx, s.config)
			m.mu.Lock()
			defer m.mu.Unlock()
			if err != nil {
				s.err = err.Error()
				log.Printf("[upstream] FAILED to connect %s: %v", name, err)
				return
			}
			s.session = session
			s.err = ""
			log.Printf("[upstream] connected: %s", name)
		}(name)
	}
	wg.Wait()
}

func (m *Manager) Status() []Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	statuses := make([]Status, 0, len(m.order))
	for _, name := range m.order {
		s := m.upstreams[name]
		statuses = append(statuses, Status{Name: name, Connected: s.session != nil, Error: s.err})
	}
	return statuses
}

// Tools returns the aggregated tool list across all connected upstreams,
// cached briefly so bursts of requests do not hammer the upstreams.
func (m *Manager) Tools(ctx context.Context) []Tool {
	m.mu.Lock()
	if time.Since(m.toolsFetched) < toolCacheTTL && m.toolsCache != nil {
		cached := m.toolsCache
		m.mu.Unlock()
		return cached
	}
	sessions := map[string]*mcp.ClientSession{}
	for _, name := range m.order {
		if s := m.upstreams[name]; s.session != nil {
			sessions[name] = s.session
		}
	}
	m.mu.Unlock()

	var (
		wg      sync.WaitGroup
		toolsMu sync.Mutex
		tools   []Tool
	)
	for name, session := range sessions {
		wg.Add(1)
		go func(name string, session *mcp.ClientSession) {
			defer wg.Done()
			listCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			result, err := session.ListTools(listCtx, nil)
			if err != nil {
				log.Printf("[upstream] listTools failed for %s: %v", name, err)
				return
			}
			toolsMu.Lock()
			defer toolsMu.Unlock()
			for _, tool := range result.Tools {
				tools = append(tools, Tool{
					Upstream:    name,
					Name:        tool.Name,
					Description: tool.Description,
					InputSchema: tool.InputSchema,
				})
			}
		}(name, session)
	}
	wg.Wait()

	sort.Slice(tools, func(i, j int) bool {
		if tools[i].Upstream != tools[j].Upstream {
			return tools[i].Upstream < tools[j].Upstream
		}
		return tools[i].Name < tools[j].Name
	})

	m.mu.Lock()
	m.toolsCache = tools
	m.toolsFetched = time.Now()
	m.mu.Unlock()
	return tools
}

func (m *Manager) CallTool(ctx context.Context, upstream, tool string, arguments any) (*mcp.CallToolResult, error) {
	m.mu.Lock()
	s, ok := m.upstreams[upstream]
	var session *mcp.ClientSession
	var lastErr string
	if ok {
		session = s.session
		lastErr = s.err
	}
	m.mu.Unlock()

	if !ok {
		return nil, fmt.Errorf("unknown upstream %q", upstream)
	}
	if session == nil {
		if lastErr == "" {
			lastErr = "unknown error"
		}
		return nil, fmt.Errorf("upstream %q is not connected (%s)", upstream, lastErr)
	}
	return session.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: arguments})
}

func (m *Manager) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, name := range m.order {
		if s := m.upstreams[name]; s.session != nil {
			_ = s.session.Close()
			s.session = nil
		}
	}
}
