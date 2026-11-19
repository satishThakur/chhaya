package dream

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCPGateway is the dreamer's connection to the gateway: an MCP session over mTLS with the
// dreamer's own certificate. It exposes exactly the three calls the gateway lets that actor make.
type MCPGateway struct{ sess *mcp.ClientSession }

// Connect opens a session. certDir holds ca.crt, dreamer.crt and dreamer.key; the gateway's
// certificate is verified against the CA, which is not skipped.
func Connect(ctx context.Context, endpoint, certDir string) (*MCPGateway, error) {
	cert, err := tls.LoadX509KeyPair(filepath.Join(certDir, "dreamer.crt"), filepath.Join(certDir, "dreamer.key"))
	if err != nil {
		return nil, fmt.Errorf("the dreamer's certificate (deploy/issue-client-cert.sh dreamer): %w", err)
	}
	caPEM, err := os.ReadFile(filepath.Join(certDir, "ca.crt"))
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("no certificate in %s/ca.crt", certDir)
	}
	hc := &http.Client{Timeout: 60 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{
		Certificates: []tls.Certificate{cert}, RootCAs: pool, MinVersion: tls.VersionTLS13}}}
	return NewMCPGateway(ctx, endpoint, hc)
}

// NewMCPGateway opens a session over a caller-supplied HTTP client (tests give it their own).
func NewMCPGateway(ctx context.Context, endpoint string, hc *http.Client) (*MCPGateway, error) {
	c := mcp.NewClient(&mcp.Implementation{Name: "chhaya-dreamer", Version: "1"}, nil)
	s, err := c.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: endpoint, HTTPClient: hc}, nil)
	if err != nil {
		return nil, fmt.Errorf("connecting to the gateway: %w", err)
	}
	return &MCPGateway{sess: s}, nil
}

func (g *MCPGateway) Close() error { return g.sess.Close() }

// call runs one tool and returns the tool's value. The gateway wraps results as {trust, value}; a
// refusal arrives as a tool error and is returned as an error carrying its text.
func (g *MCPGateway) call(ctx context.Context, name string, args map[string]any) (json.RawMessage, error) {
	res, err := g.sess.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return nil, err
	}
	var text strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			text.WriteString(tc.Text)
		}
	}
	if res.IsError {
		return nil, fmt.Errorf("%s", text.String())
	}
	var env struct {
		Value json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal([]byte(text.String()), &env); err != nil {
		return nil, fmt.Errorf("unexpected reply from %s", name)
	}
	return env.Value, nil
}

func (g *MCPGateway) List(ctx context.Context) ([]string, error) {
	v, err := g.call(ctx, "memory.list", map[string]any{})
	if err != nil {
		return nil, err
	}
	var out struct {
		Documents []struct {
			Path string `json:"path"`
		} `json:"documents"`
		Truncated bool `json:"truncated"`
	}
	if err := json.Unmarshal(v, &out); err != nil {
		return nil, err
	}
	if out.Truncated {
		return nil, fmt.Errorf("the memory list was truncated; the dreamer will not guess at what it cannot see")
	}
	var paths []string
	for _, d := range out.Documents {
		paths = append(paths, d.Path)
	}
	return paths, nil
}

func (g *MCPGateway) Get(ctx context.Context, path string) (string, error) {
	v, err := g.call(ctx, "memory.get", map[string]any{"path": path})
	if err != nil {
		return "", err
	}
	var s string
	if err := json.Unmarshal(v, &s); err != nil {
		return "", err
	}
	return s, nil
}

func (g *MCPGateway) Propose(ctx context.Context, path, entry, rationale string) error {
	_, err := g.call(ctx, "memory.propose_update", map[string]any{"path": path, "entry": entry, "rationale": rationale})
	return err
}
