// chhayad is the policy gateway: the only MCP server Hermes can see, and the
// only door to code, money and data.
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/satishthakur/chhaya/internal/approval"
	"github.com/satishthakur/chhaya/internal/audit"
	"github.com/satishthakur/chhaya/internal/config"
	"github.com/satishthakur/chhaya/internal/mcpsrv"
	"github.com/satishthakur/chhaya/internal/memory"
	"github.com/satishthakur/chhaya/internal/state"
	"github.com/satishthakur/chhaya/internal/tools"
)

const version = "0.1.0"

func main() {
	if err := run(); err != nil {
		log.Fatalf("chhayad: %v", err)
	}
}

func run() error {
	dataDir := env("CHHAYA_DATA_DIR", "/var/lib/chhaya")
	policyFile := env("CHHAYA_POLICY_FILE", "/etc/chhaya/policy.yaml")
	memoryRoot := env("CHHAYA_MEMORY_ROOT", filepath.Join(dataDir, "memory"))
	addr := env("CHHAYA_LISTEN", "0.0.0.0:8443")

	// Step 0. The two bots' secrets must not have been crossed. This runs
	// before anything is read or created, so the one misconfiguration that
	// would silently void the safety model still fails first and loudest.
	if err := mcpsrv.CheckEnvironmentIsolation(os.Getenv); err != nil {
		return err
	}
	// The audit log and the state file both live here and neither creates it.
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}
	chatID, err := strconv.ParseInt(strings.TrimSpace(os.Getenv("CHHAYA_APPROVER_ALLOWED_USERS")), 10, 64)
	if err != nil {
		return fmt.Errorf("CHHAYA_APPROVER_ALLOWED_USERS must be one numeric Telegram user id: %w", err)
	}

	// Step 1. Policy. A missing value is the restrictive reading, not a default.
	cfg, policyHash, err := config.Load(policyFile)
	if err != nil {
		return err
	}
	log.Printf("policy loaded, hash %s", policyHash[:12])

	// Step 2. Audit log. A broken chain is a refusal, not a warning.
	w, err := audit.Open(filepath.Join(dataDir, "audit.jsonl"))
	if err != nil {
		return err
	}
	defer w.Close()

	// Step 6 (early, because the bot needs it). Persisted kill switch and taint.
	st, err := state.Open(filepath.Join(dataDir, "state.json"))
	if err != nil {
		return err
	}
	if st.Snapshot(time.Now()).KillSwitch {
		log.Print("kill switch is ON from a previous /stop; writes will be refused")
	}

	// Steps 3 and 4. The token must work and the chat must be reachable.
	bot := approval.NewBot(
		approval.NewHTTPTelegram(os.Getenv("CHHAYA_APPROVER_BOT_TOKEN"), nil),
		chatID, st, w, cfg.ApprovalTimeout, cfg, memory.Store{Root: memoryRoot},
	)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := bot.Preflight(ctx); err != nil {
		return err
	}

	// Step 5. A card only on first boot or a policy change.
	markFile := filepath.Join(dataDir, "policy.hash")
	prev, err := os.ReadFile(markFile)
	if err != nil && !os.IsNotExist(err) {
		// Not fatal — "no prior boot" and "can't tell" both fall back to
		// posting the card, which is the safe side to err on — but silently
		// treating an unreadable mark file as first boot would hide a real
		// filesystem problem in dataDir.
		log.Printf("could not read %s: %v; treating this as first boot", markFile, err)
	}
	if string(prev) != policyHash {
		if err := bot.SendStartupCard(ctx, policyHash, len(prev) == 0); err != nil {
			return fmt.Errorf("the approver chat accepted getChat but refused a message, "+
				"which usually means the bot is blocked: %w", err)
		}
		if err := os.WriteFile(markFile, []byte(policyHash), 0o600); err != nil {
			return err
		}
	}

	// Every pending proposal gets a fresh card: the cards from before this
	// start are tied to a process that is gone. Not fatal: the proposals stay
	// on disk and are re-posted on the next start, and refusing to boot would
	// also take away /stop.
	if err := bot.RepostPending(ctx); err != nil {
		log.Printf("could not re-post pending memory proposals: %v; they stay on disk for the next start", err)
	}

	go func() {
		err := bot.Run(ctx)
		if ctx.Err() != nil {
			log.Print("approver bot stopped: shutting down")
			return
		}
		log.Printf("approver bot stopped unexpectedly: %v", err)
	}()

	// Build the gateway. Describe is a closure because capabilities needs the
	// registration list the gateway is being constructed from.
	var g *mcpsrv.Gateway
	regs := tools.All(tools.Deps{
		Version: version, MemoryRoot: memoryRoot, State: st, Config: cfg,
		Notifier: bot, // a tap on its card is the only path that writes a memory document
		Describe: func() []mcpsrv.Registration { return g.Registrations() },
	})
	g, err = mcpsrv.New(cfg, st, w, bot, regs)
	if err != nil {
		return err
	}

	// Step 7. Only now bind. Hermes may connect before this and will retry;
	// a gateway that accepts calls before it can ask for approval fails open.
	tlsCfg, err := mcpsrv.TLSConfig(
		env("CHHAYA_TLS_CERT", "/etc/chhaya/certs/gateway.crt"),
		env("CHHAYA_TLS_KEY", "/etc/chhaya/certs/gateway.key"),
		env("CHHAYA_TLS_CA", "/etc/chhaya/certs/ca.crt"),
	)
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.Handle("/mcp", g.MCPHandler("chhaya-gateway", version))
	srv := &http.Server{
		Addr: addr, Handler: mux, TLSConfig: tlsCfg,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		<-ctx.Done()
		sc, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(sc)
	}()

	log.Printf("listening on %s with mTLS, %d tools registered", addr, len(regs))
	if err := srv.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
