package mcpsrv

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// CheckEnvironmentIsolation is startup step 0. TELEGRAM_BOT_TOKEN belongs to
// Hermes; its presence in the gateway's environment means the wrong env file
// was mounted and the two bots' secrets have been crossed. The check needs no
// knowledge of any token's value — presence alone is the error.
//
// This is the one configuration mistake that would silently void the safety
// model, so it is the one that must fail loudly at boot. A rule enforced in
// code beats a rule written in a comment.
//
// There is deliberately no separate "the two tokens are the same value"
// check: that case is already a TELEGRAM_BOT_TOKEN-is-set failure, since
// equality requires both to be non-empty and the first branch below returns
// before an equality check could ever run. A third branch testing for it
// would be unreachable — dead code asserting a distinction this function
// cannot actually draw.
func CheckEnvironmentIsolation(getenv func(string) string) error {
	approver := getenv("CHHAYA_APPROVER_BOT_TOKEN")
	hermes := getenv("TELEGRAM_BOT_TOKEN")

	if hermes != "" {
		return errors.New("TELEGRAM_BOT_TOKEN is set in the gateway's environment. " +
			"That variable belongs to Hermes; its presence here means deploy/.env.hermes was mounted into the gateway. " +
			"If the approver token ever reaches Hermes, the agent can forge its own approvals")
	}
	if approver == "" {
		return errors.New("CHHAYA_APPROVER_BOT_TOKEN is not set; without it there is no approval channel and no kill switch")
	}
	return nil
}

// TLSConfig builds the server side of mTLS. Hermes presents a client
// certificate signed by our CA, or it does not get in.
func TLSConfig(certFile, keyFile, caFile string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load server keypair: %w", err)
	}
	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("no certificates found in %s", caFile)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
		MinVersion:   tls.VersionTLS13,
	}, nil
}

// MCPHandler exposes the registry over MCP. Every tool is registered with the
// same handler shape: unmarshal, call Invoke, marshal. No tool gets its own
// path to its handler, so the choke point survives contact with the SDK.
func (g *Gateway) MCPHandler(name, version string) http.Handler {
	srv := mcp.NewServer(&mcp.Implementation{Name: name, Version: version}, nil)

	for _, reg := range g.tools {
		reg := reg
		srv.AddTool(&mcp.Tool{
			Name:        reg.Name,
			Description: reg.Description,
			InputSchema: reg.Schema,
		}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			args := map[string]any{}
			if len(req.Params.Arguments) > 0 {
				if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
					return errorResult("arguments were not a JSON object"), nil
				}
			}

			res := g.Invoke(ctx, ToolCall{Actor: "hermes", Name: req.Params.Name, Args: args})
			if res.IsError {
				return errorResult(res.Error), nil
			}

			body, err := json.Marshal(map[string]any{"trust": string(res.Trust), "value": res.Value})
			if err != nil {
				return errorResult("result was not serialisable"), nil
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(body)}}}, nil
		})
	}

	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
}

// errorResult returns a tool error rather than a protocol error, so the model
// can see the refusal and correct itself instead of the call simply failing.
func errorResult(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: msg}}}
}
