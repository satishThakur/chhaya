package mcpsrv

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/satishthakur/chhaya/internal/audit"
	"github.com/satishthakur/chhaya/internal/policy"
	"github.com/satishthakur/chhaya/internal/state"
)

var errUnreachable = errors.New("approver unreachable")

// Approver asks the human. It is an interface so that internal/mcpsrv does not
// depend on Telegram, and so the tests above need no network.
type Approver interface {
	Request(ctx context.Context, call ToolCall, d policy.Decision) (approved bool, latency time.Duration, err error)
}

type ToolCall struct {
	Actor string // "hermes"
	Name  string
	Args  map[string]any
}

type ToolResult struct {
	Value   any
	Trust   Trust
	IsError bool
	Error   string
	RuleID  string
}

type Gateway struct {
	cfg       policy.Config
	state     *state.Store
	audit     *audit.Writer
	auditPath string
	approver  Approver
	tools     registry
	now       func() time.Time
}

func New(cfg policy.Config, st *state.Store, w *audit.Writer, ap Approver, regs []Registration) (*Gateway, error) {
	reg, err := buildRegistry(regs)
	if err != nil {
		return nil, err
	}
	if ap == nil {
		return nil, errors.New("no approver: a gateway that cannot ask cannot approve")
	}
	return &Gateway{
		cfg: cfg, state: st, audit: w, auditPath: w.Path(),
		approver: ap, tools: reg, now: time.Now,
	}, nil
}

// Registrations returns tool metadata with handlers stripped. capabilities()
// and chhayactl both need to describe the tools; neither may call them.
func (g *Gateway) Registrations() []Registration {
	out := make([]Registration, 0, len(g.tools))
	for _, r := range g.tools {
		r.Handle, r.Validate = nil, nil
		out = append(out, r)
	}
	return out
}

// Invoke is the only exported path to a tool handler. It always runs, in order:
// policy.Decide → dispatch-or-refuse → audit.Append. There is no branch that
// skips the first or the last.
func (g *Gateway) Invoke(ctx context.Context, call ToolCall) ToolResult {
	start := g.now()
	reg, known := g.tools[call.Name]

	req := policy.Request{
		Tool:   call.Name,
		Args:   call.Args,
		Known:  known,
		Kind:   reg.Kind,
		Tier:   reg.Tier,
		CapKey: reg.CapKey,
	}
	if known && reg.Tier == policy.TierReversible {
		if err := reg.Validate(call.Args); err != nil {
			req.ValidatorErr = err.Error()
		} else {
			req.ValidatorOK = true
		}
	} else {
		req.ValidatorOK = true // only tier 1 consults it
	}

	st := g.state.Snapshot(start)
	d := policy.Decide(req, st, g.cfg, start)

	switch d.Outcome {
	case policy.Deny:
		return g.refuse(call, reg, st, d, start, "")

	case policy.NeedApproval:
		g.record(call, reg, st, d, "needs_approval", nil, audit.Result{Status: "pending"}, start, "")
		approved, latency, err := g.approver.Request(ctx, call, d)
		if err != nil {
			// The safety channel itself is down. Refuse loudly, and tell Hermes
			// in a way it can repeat to the user: the agent becomes the
			// out-of-band alarm precisely when the alarm is what is broken.
			ans := &audit.Approval{Answer: "channel_failed", LatencyMS: latency.Milliseconds()}
			g.record(call, reg, st, d, "deny", ans,
				audit.Result{Status: "refused", Error: err.Error()}, start, "critical")
			return ToolResult{
				IsError: true, RuleID: "approval.unreachable",
				Error: "refused: the approval channel is unreachable, so this cannot be approved. " +
					"Tell the user their approver bot is not responding.",
			}
		}
		if !approved {
			ans := &audit.Approval{Answer: "denied_by_user", LatencyMS: latency.Milliseconds()}
			g.record(call, reg, st, d, "deny", ans, audit.Result{Status: "refused"}, start, "")
			return ToolResult{IsError: true, RuleID: d.RuleID, Error: "refused: you declined this"}
		}
		res := g.dispatch(ctx, reg, call, start)
		ans := &audit.Approval{Answer: "approved", LatencyMS: latency.Milliseconds()}
		g.record(call, reg, st, d, "allow", ans, resultOf(res), start, "")
		return res

	default: // policy.Allow
		res := g.dispatch(ctx, reg, call, start)
		g.record(call, reg, st, d, "allow", nil, resultOf(res), start, "")
		return res
	}
}

func (g *Gateway) dispatch(ctx context.Context, reg Registration, call ToolCall, start time.Time) ToolResult {
	if err := g.state.RecordCall(call.Name, start); err != nil {
		return ToolResult{IsError: true, RuleID: "state.unwritable",
			Error: "refused: the gateway cannot record this call, so it will not make it"}
	}
	v, err := reg.Handle(ctx, call.Args)
	if err != nil {
		return ToolResult{Trust: reg.Trust, IsError: true, Error: err.Error()}
	}
	// Trust is stamped here, from the registration. The handler had no say.
	if reg.Trust == Untrusted {
		if err := g.state.SetTaint(true, reg.Name, g.now()); err != nil {
			return ToolResult{IsError: true, RuleID: "state.unwritable",
				Error: "refused: untrusted content could not be marked as such, so it is not returned"}
		}
	}
	return ToolResult{Value: v, Trust: reg.Trust}
}

func (g *Gateway) refuse(call ToolCall, reg Registration, st policy.State, d policy.Decision, start time.Time, sev string) ToolResult {
	g.record(call, reg, st, d, "deny", nil, audit.Result{Status: "refused"}, start, sev)
	return ToolResult{IsError: true, RuleID: d.RuleID, Error: d.Reason}
}

func (g *Gateway) record(call ToolCall, reg Registration, st policy.State, d policy.Decision,
	decision string, ap *audit.Approval, res audit.Result, start time.Time, severity string) {

	// MarshalArgs, not json.Marshal: the audit package owns its encoding mode
	// so that the bytes written are the bytes hashed. The redact paths are
	// copied into a fresh slice rather than appended onto reg.Redact directly:
	// reg is read from the registry on every call, so appending onto its slice
	// in place would race with a concurrent Invoke of the same tool.
	redactPaths := make([]string, 0, len(reg.Redact)+len(d.Redact))
	redactPaths = append(redactPaths, reg.Redact...)
	redactPaths = append(redactPaths, d.Redact...)
	args := audit.MarshalArgs(audit.Redact(call.Args, redactPaths))

	r := audit.Record{
		Actor: orDefault(call.Actor, "unknown"), Tool: call.Name,
		Kind: reg.Kind.String(), Tier: int(d.Tier),
		Args: args, Decision: decision, Rule: d.RuleID, Reason: d.Reason,
		Taint:      audit.Taint{Tainted: st.Tainted, Source: st.TaintSource},
		Approval:   ap,
		Result:     res,
		DurationMS: g.now().Sub(start).Milliseconds(),
		Severity:   severity,
	}
	if _, err := g.audit.Append(r); err != nil {
		// This does not retroactively undo a dispatch that already ran, and it
		// does not make later calls refuse either — there is no record of the
		// failure beyond this line. stderr is the only place left to put it.
		fmt.Fprintf(os.Stderr, "CRITICAL: audit append failed: %v\n", err)
	}
}

func resultOf(r ToolResult) audit.Result {
	if r.IsError {
		return audit.Result{Status: "error", Error: r.Error}
	}
	return audit.Result{Status: "ok"}
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}
