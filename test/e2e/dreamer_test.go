//go:build e2e

package e2e

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/satishthakur/chhaya/internal/audit"
	"github.com/satishthakur/chhaya/internal/dream"
	"github.com/satishthakur/chhaya/internal/mcpsrv"
	"github.com/satishthakur/chhaya/internal/memory"
	"github.com/satishthakur/chhaya/internal/policy"
	"github.com/satishthakur/chhaya/internal/state"
	"github.com/satishthakur/chhaya/internal/tools"
)

// A throwaway PKI, made in the test so it does not depend on deploy/certs having a dreamer cert.
type pki struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

func newCA(t *testing.T, cn string) pki {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := x509.ParseCertificate(der)
	return pki{c, key, pemOf(der)}
}

func pemOf(der []byte) []byte { return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}) }

func (ca pki) issue(t *testing.T, cn string, server bool) tls.Certificate {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature}
	if server {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		tmpl.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
	} else {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

type notifier struct{ sent []memory.Proposal }

func (n *notifier) ProposeMemory(_ context.Context, p memory.Proposal) error {
	n.sent = append(n.sent, p)
	return nil
}

type fixture struct {
	url     string
	ca      pki
	logPath string
	memRoot string
	notes   *notifier
}

func startGateway(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	st, err := state.Open(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	w, err := audit.Open(filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.Close() })
	memRoot := filepath.Join(dir, "memory")
	os.MkdirAll(filepath.Join(memRoot, ".tests"), 0o700)
	os.WriteFile(filepath.Join(memRoot, "interests.md"), []byte("# Interests\n\n- Loves books. <!-- proposal 20261106-3f609264ada59fd2, approved 2026-11-06 -->\n"), 0o600)
	os.WriteFile(filepath.Join(memRoot, ".tests", "probes.json"), []byte(`[{"id":"x"}]`), 0o600)

	cfg := policy.Config{DefaultRatePerHour: 60, ApprovalTimeout: time.Minute, BranchPrefix: "agent/",
		Caps:    map[string]policy.Cap{"food": {PerOrderMinor: 80000, PerDayMinor: 150000}},
		Dreamer: policy.DreamerLimits{ProposalsPerDay: 2, CallsPerHour: 100}}
	notes := &notifier{}
	var g *mcpsrv.Gateway
	regs := tools.All(tools.Deps{Version: "e2e", MemoryRoot: memRoot, State: st, Config: cfg, Notifier: notes,
		Describe: func() []mcpsrv.Registration { return g.Registrations() }})
	g, err = mcpsrv.New(cfg, st, w, refusingApprover{}, regs)
	if err != nil {
		t.Fatal(err)
	}
	ca := newCA(t, "test-ca")
	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)
	mux := http.NewServeMux()
	mux.Handle("/mcp", g.MCPHandler("chhaya-gateway", "e2e"))
	srv := httptest.NewUnstartedServer(mux)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{ca.issue(t, "chhaya-gateway", true)},
		ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool, MinVersion: tls.VersionTLS13}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return &fixture{url: srv.URL + "/mcp", ca: ca, logPath: filepath.Join(dir, "audit.jsonl"), memRoot: memRoot, notes: notes}
}

type addHeader struct {
	rt http.RoundTripper
	k  string
	v  string
}

func (a addHeader) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set(a.k, a.v)
	return a.rt.RoundTrip(r)
}

// connect opens an MCP session with cert; spoof, if set, is a header the client tries to add.
func (f *fixture) connect(t *testing.T, cert tls.Certificate, spoof string) (*mcp.ClientSession, error) {
	t.Helper()
	var rt http.RoundTripper = &http.Transport{TLSClientConfig: &tls.Config{Certificates: []tls.Certificate{cert}, InsecureSkipVerify: true}} // the test targets the server's check of the client
	if spoof != "" {
		rt = addHeader{rt, "X-Chhaya-Actor", spoof}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c := mcp.NewClient(&mcp.Implementation{Name: "e2e-client", Version: "0"}, nil)
	return c.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: f.url, HTTPClient: &http.Client{Transport: rt}}, nil)
}

func callTool(t *testing.T, s *mcp.ClientSession, name string, args map[string]any) (string, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: a refusal must arrive as a tool error: %v", name, err)
	}
	return contentText(res), res.IsError
}

func lastActorRecord(t *testing.T, f *fixture, tool string) audit.Record {
	t.Helper()
	recs := readLog(t, f.logPath)
	for i := len(recs) - 1; i >= 0; i-- {
		if recs[i].Tool == tool {
			return recs[i]
		}
	}
	t.Fatalf("no audit record for %s", tool)
	return audit.Record{}
}

func TestDreamerOverRealMTLS(t *testing.T) {
	f := startGateway(t)
	dreamer := f.ca.issue(t, "dreamer", false)
	s, err := f.connect(t, dreamer, "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// What it may do.
	if out, isErr := callTool(t, s, "memory.list", nil); isErr || !strings.Contains(out, "interests") {
		t.Fatalf("memory.list: %q %v", out, isErr)
	}
	if out, isErr := callTool(t, s, "memory.get", map[string]any{"path": "interests"}); isErr || !strings.Contains(out, "Loves books") {
		t.Fatalf("memory.get: %q %v", out, isErr)
	}
	if rec := lastActorRecord(t, f, "memory.get"); rec.Actor != "dreamer" {
		t.Fatalf("the audit log must say dreamer, said %q", rec.Actor)
	}

	// It cannot read the memory's own tests (the memory layer refuses dot-directories).
	if out, isErr := callTool(t, s, "memory.get", map[string]any{"path": ".tests/probes"}); !isErr {
		t.Fatalf("the dreamer read the tests: %q", out)
	}

	// Every other tool is refused by name, and nothing it would have done happened.
	for _, tool := range []string{"capabilities", "ping"} {
		if out, isErr := callTool(t, s, tool, map[string]any{}); !isErr {
			t.Errorf("%s was allowed for the dreamer: %q", tool, out)
		}
	}
	// Tools that do not exist (a merge, a revert, an approval, a write to the tests) cannot be named
	// at all: the protocol refuses before the gateway is asked.
	for _, tool := range []string{"memory.propose_remove", "health.get_summary", "health.get_series", "health.get_digest", "memory.merge", "memory.revert", "memory.approve", "memory.write", "memory.put_test", "swiggy.order"} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: map[string]any{}})
		cancel()
		if err == nil && !res.IsError {
			t.Errorf("%s was allowed for the dreamer", tool)
		}
	}
	if rec := lastActorRecord(t, f, "capabilities"); rec.Decision != "deny" || rec.Rule != "actor.tool_not_allowed" || rec.Actor != "dreamer" {
		t.Fatalf("%+v", rec)
	}

	// Proposals: two within the cap reach the approver; the third is refused before it does.
	prop := map[string]any{"path": "interests", "entry": "Has already heard X.", "rationale": "owner said so (trace t, msg 3)"}
	for i := 0; i < 2; i++ {
		prop["entry"] = "Has already heard X" + string(rune('A'+i)) + "."
		if out, isErr := callTool(t, s, "memory.propose_update", prop); isErr {
			t.Fatalf("proposal %d: %q", i+1, out)
		}
	}
	prop["entry"] = "Has already heard Z."
	if out, isErr := callTool(t, s, "memory.propose_update", prop); !isErr || !strings.Contains(out, "limit is 2") {
		t.Fatalf("the third proposal must be refused by the cap: %q %v", out, isErr)
	}
	if len(f.notes.sent) != 2 {
		t.Fatalf("the approver saw %d proposals, want 2", len(f.notes.sent))
	}
	// Nothing was written to a memory document: only a tap does that.
	if b, _ := os.ReadFile(filepath.Join(f.memRoot, "interests.md")); strings.Contains(string(b), "heard X") {
		t.Fatal("a proposal reached the memory document without a tap")
	}
}

func TestADreamerCannotPassAsHermesByAHeaderOrACertFromAnotherCA(t *testing.T) {
	f := startGateway(t)
	s, err := f.connect(t, f.ca.issue(t, "dreamer", false), "hermes")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if out, isErr := callTool(t, s, "capabilities", nil); !isErr {
		t.Fatalf("a spoofed actor header got hermes' tools: %q", out)
	}
	if rec := lastActorRecord(t, f, "capabilities"); rec.Actor != "dreamer" || rec.Decision != "deny" {
		t.Fatalf("%+v", rec)
	}

	// A certificate that names hermes but was not signed by our CA never gets a session.
	other := newCA(t, "someone-else")
	if s2, err := f.connect(t, other.issue(t, "hermes", false), ""); err == nil {
		s2.Close()
		t.Fatal("a client certificate from another CA was accepted")
	}
}

func TestAnActorOurCAIssuedButPolicyDoesNotKnowIsRefused(t *testing.T) {
	f := startGateway(t)
	s, err := f.connect(t, f.ca.issue(t, "intruder", false), "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if out, isErr := callTool(t, s, "memory.get", map[string]any{"path": "interests"}); !isErr || strings.Contains(out, "Loves books") {
		t.Fatalf("%q %v", out, isErr)
	}
	if rec := lastActorRecord(t, f, "memory.get"); rec.Rule != "actor.unknown" || rec.Actor != "intruder" {
		t.Fatalf("%+v", rec)
	}
}

func TestHermesKeepsItsTools(t *testing.T) {
	f := startGateway(t)
	s, err := f.connect(t, f.ca.issue(t, "hermes", false), "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if out, isErr := callTool(t, s, "capabilities", nil); isErr {
		t.Fatalf("%q", out)
	}
	if rec := lastActorRecord(t, f, "capabilities"); rec.Actor != "hermes" || rec.Decision != "allow" {
		t.Fatalf("%+v", rec)
	}
}

// The dreamer's own client (internal/dream.MCPGateway), against the real gateway over mTLS.
func TestTheDreamerClientListsReadsAndProposes(t *testing.T) {
	f := startGateway(t)
	dreamer := f.ca.issue(t, "dreamer", false)
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{Certificates: []tls.Certificate{dreamer}, InsecureSkipVerify: true}}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	gw, err := dream.NewMCPGateway(ctx, f.url, hc)
	if err != nil {
		t.Fatal(err)
	}
	defer gw.Close()
	paths, err := gw.List(ctx)
	if err != nil || len(paths) != 1 || paths[0] != "interests" {
		t.Fatalf("%v %v", paths, err)
	}
	body, err := gw.Get(ctx, "interests")
	if err != nil || !strings.Contains(body, "Loves books") {
		t.Fatalf("%q %v", body, err)
	}
	if _, err := gw.Get(ctx, ".tests/probes"); err == nil {
		t.Fatal("the dreamer's client read the memory tests")
	}
	if err := gw.Propose(ctx, "interests", "Has heard Foo.", "said so (trace t, msg 1)"); err != nil {
		t.Fatal(err)
	}
	if len(f.notes.sent) != 1 || f.notes.sent[0].Entry != "Has heard Foo." {
		t.Fatalf("the approver saw %+v", f.notes.sent)
	}
	// the cap in this fixture is 2: a third is refused with the gateway's own words
	gw.Propose(ctx, "interests", "Has heard Bar.", "x")
	if err := gw.Propose(ctx, "interests", "Has heard Baz.", "x"); err == nil || !strings.Contains(err.Error(), "limit is 2") {
		t.Fatalf("%v", err)
	}
}
