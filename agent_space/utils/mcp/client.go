// Package mcp connects the agent to the CockroachDB Cloud Managed MCP Server and exposes its tools as both tools.Tool and a schema-carrying form.
package mcp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sort"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"agent_space/utils"
)

// the managed MCP endpoint; the server speaks streamable HTTP, not SSE
const DefaultURL = "https://cockroachlabs.cloud/mcp"

// scopes every request to one cluster; omitting it widens access to every cluster the key permits
const clusterIDHeader = "mcp-cluster-id"

// bounds the handshake at boot so a wrong URL fails startup quickly
const connectTimeout = 30 * time.Second

// ceiling on any single request; must stay above the server's own 20s query cap
const requestTimeout = 90 * time.Second

// no API key supplied; callers treat this as "run without MCP" so the vector-only routes stay up
var ErrNotConfigured = errors.New("mcp: COCKROACH_API_KEY is not set")

// whether the session itself is unusable, as opposed to the server refusing one call. Policy rejections come back as JSON-RPC errors and are recoverable, so only known-fatal cases count.
func IsSessionFailure(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, sdk.ErrConnectionClosed) || errors.Is(err, sdk.ErrSessionMissing) {
		return true
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	// http.Client.Timeout is a *url.Error that does not always wrap context.DeadlineExceeded
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// everything needed to reach the managed MCP server
type Config struct {
	URL       string
	APIKey    string
	ClusterID string
}

// MCP settings, using the same envOr convention as the LLM configuration
func ConfigFromEnv() Config {
	return Config{
		URL:       utils.EnvOr("COCKROACH_MCP_URL", DefaultURL),
		APIKey:    utils.EnvOr("COCKROACH_API_KEY", ""),
		ClusterID: utils.EnvOr("COCKROACH_CLUSTER_ID", ""),
	}
}

// whether there is enough information to attempt a connection
func (c Config) Configured() bool {
	return c.APIKey != "" && c.URL != ""
}

// injects auth and cluster-scope headers; the SDK's transport exposes an http.Client but no header hook
type headerRoundTripper struct {
	base      http.RoundTripper
	apiKey    string
	clusterID string
}

func (h *headerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	// RoundTrippers must not mutate the request they are given.
	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", "Bearer "+h.apiKey)
	if h.clusterID != "" {
		clone.Header.Set(clusterIDHeader, h.clusterID)
	}

	base := h.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(clone)
}

// a live connection plus the tools it advertised at connect time
type Session struct {
	config  Config
	session *sdk.ClientSession
	tools   []*Tool
	byName  map[string]*Tool
}

// handshake and tool discovery; an error means the agent runs without MCP, never fatal to the process
func Connect(ctx context.Context, cfg Config) (*Session, error) {
	if !cfg.Configured() {
		return nil, ErrNotConfigured
	}

	httpClient := &http.Client{
		Transport: &headerRoundTripper{apiKey: cfg.APIKey, clusterID: cfg.ClusterID},
		Timeout:   requestTimeout,
	}

	client := sdk.NewClient(&sdk.Implementation{
		Name:    "agent_space",
		Version: "0.1.0",
	}, nil)

	// we only ever call tools, so the persistent GET stream is pure liability
	transport := &sdk.StreamableClientTransport{
		Endpoint:             cfg.URL,
		HTTPClient:           httpClient,
		DisableStandaloneSSE: true,
	}

	connCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()

	cs, err := client.Connect(connCtx, transport, nil)
	if err != nil {
		return nil, fmt.Errorf("mcp: connect to %s: %w", cfg.URL, err)
	}

	s := &Session{config: cfg, session: cs, byName: map[string]*Tool{}}
	if err := s.discover(connCtx); err != nil {
		_ = cs.Close()
		return nil, err
	}

	return s, nil
}

// discover walks the paginated tools/list response and wraps each entry.
func (s *Session) discover(ctx context.Context) error {
	var cursor string
	for {
		res, err := s.session.ListTools(ctx, &sdk.ListToolsParams{Cursor: cursor})
		if err != nil {
			return fmt.Errorf("mcp: list tools: %w", err)
		}

		for _, t := range res.Tools {
			tool := &Tool{remote: t, session: s}
			s.tools = append(s.tools, tool)
			s.byName[t.Name] = tool
		}

		if res.NextCursor == "" {
			break
		}
		cursor = res.NextCursor
	}

	if len(s.tools) == 0 {
		return errors.New("mcp: server advertised no tools")
	}

	sort.Slice(s.tools, func(i, j int) bool { return s.tools[i].remote.Name < s.tools[j].remote.Name })
	return nil
}

// every discovered tool, sorted by name
func (s *Session) Tools() []*Tool { return s.tools }

// Tool looks up a single tool by its MCP name.
func (s *Session) Tool(name string) (*Tool, bool) {
	t, ok := s.byName[name]
	return t, ok
}

// the discovered tool names, for logging
func (s *Session) Names() []string {
	names := make([]string, 0, len(s.tools))
	for _, t := range s.tools {
		names = append(names, t.remote.Name)
	}
	return names
}

// the URL this session talks to; safe to log, it carries no credential
func (s *Session) Endpoint() string { return s.config.URL }

// the cluster this session is scoped to, or "" for org-wide
func (s *Session) ClusterID() string { return s.config.ClusterID }

// Close terminates the MCP session.
func (s *Session) Close() error {
	if s == nil || s.session == nil {
		return nil
	}
	return s.session.Close()
}
