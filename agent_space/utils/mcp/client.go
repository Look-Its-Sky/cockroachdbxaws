// Package mcp connects the agent to the CockroachDB Cloud Managed MCP Server
// and exposes its remote tools in the two shapes the rest of the codebase
// needs: langchaingo's tools.Tool, and a schema-carrying form the native tool
// loop can hand straight to the model.
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

// DefaultURL is the managed MCP endpoint. The server speaks streamable HTTP;
// SSE is deliberately not offered, so the transport below is the only option.
const DefaultURL = "https://cockroachlabs.cloud/mcp"

// clusterIDHeader scopes every request to a single cluster. Omitting it widens
// access to every cluster the API key's Cloud RBAC roles permit, which is more
// authority than this agent needs.
const clusterIDHeader = "mcp-cluster-id"

// connectTimeout bounds the initialize + tools/list handshake at boot, so a
// wrong URL fails startup quickly instead of hanging it.
const connectTimeout = 30 * time.Second

// requestTimeout is the ceiling on any single HTTP request, and must stay well
// above the server's own 20s query cap — http.Client.Timeout applies to every
// request, not just the handshake, so setting it to connectTimeout would cut
// off legitimate slow queries.
const requestTimeout = 90 * time.Second

// ErrNotConfigured signals that no API key was supplied. Callers treat this as
// "run without MCP" rather than a failure, so the vector-only routes stay up.
var ErrNotConfigured = errors.New("mcp: COCKROACH_API_KEY is not set")

// IsSessionFailure reports whether an error from a tool call means the session
// itself is unusable, as opposed to the server refusing that one call.
//
// The distinction decides whether a run can continue, and it cannot be made by
// asking "did CallTool return an error": the Cloud server rejects a call for
// policy reasons — a restricted schema, a malformed argument — with a JSON-RPC
// error rather than a result carrying isError. Those are recoverable, and the
// model routinely does recover by querying a different way. Treating every
// returned error as fatal aborts runs that were one tool call from an answer.
//
// So the test is inverted: fatal only for the failures known to be fatal. The
// SDK's WireError type is in an internal package, so its code cannot be
// inspected — but it exports sentinels for exactly the unrecoverable cases,
// and a timed-out or cancelled request is unrecoverable by inspection.
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
	// http.Client.Timeout surfaces as a *url.Error that does not always wrap
	// context.DeadlineExceeded, so ask the net.Error interface directly.
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// Config is everything needed to reach the managed MCP server.
type Config struct {
	URL       string
	APIKey    string
	ClusterID string
}

// ConfigFromEnv reads the MCP settings using the same envOr convention as the
// LLM configuration.
func ConfigFromEnv() Config {
	return Config{
		URL:       utils.EnvOr("COCKROACH_MCP_URL", DefaultURL),
		APIKey:    utils.EnvOr("COCKROACH_API_KEY", ""),
		ClusterID: utils.EnvOr("COCKROACH_CLUSTER_ID", ""),
	}
}

// Configured reports whether there is enough information to attempt a connection.
func (c Config) Configured() bool {
	return c.APIKey != "" && c.URL != ""
}

// headerRoundTripper injects the auth and cluster-scope headers on every
// request. The SDK's StreamableClientTransport exposes an *http.Client but no
// header hook, so decorating the transport is the only place to put these.
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

// Session is a live connection to the MCP server plus the tools it advertised
// at connect time.
type Session struct {
	config  Config
	session *sdk.ClientSession
	tools   []*Tool
	byName  map[string]*Tool
}

// Connect performs the MCP handshake and discovers the server's tools.
//
// A returned error means the agent runs without MCP; it is never fatal to the
// process, so the caller logs it and carries on.
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

	// DisableStandaloneSSE: we only ever call tools, so the persistent GET
	// stream for server-initiated messages is pure liability here.
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

// Tools returns every discovered tool, sorted by name.
func (s *Session) Tools() []*Tool { return s.tools }

// Tool looks up a single tool by its MCP name.
func (s *Session) Tool(name string) (*Tool, bool) {
	t, ok := s.byName[name]
	return t, ok
}

// Names returns the discovered tool names, for logging.
func (s *Session) Names() []string {
	names := make([]string, 0, len(s.tools))
	for _, t := range s.tools {
		names = append(names, t.remote.Name)
	}
	return names
}

// Endpoint returns the URL this session is talking to. Safe to log: it carries
// no credential.
func (s *Session) Endpoint() string { return s.config.URL }

// ClusterID returns the cluster this session is scoped to, or "" for org-wide.
func (s *Session) ClusterID() string { return s.config.ClusterID }

// Close terminates the MCP session.
func (s *Session) Close() error {
	if s == nil || s.session == nil {
		return nil
	}
	return s.session.Close()
}
