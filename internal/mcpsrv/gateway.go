package mcpsrv

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/satishthakur/chhaya/internal/audit"
	"github.com/satishthakur/chhaya/internal/policy"
	"github.com/satishthakur/chhaya/internal/state"
)

var errUnreachable = errors.New("approver unreachable")

// approvalBackstopMargin is added to cfg.ApprovalTimeout to bound the
// approver call in Invoke. It is a var rather than a literal only so a test
// can shrink it to prove the backstop actually ends a hung approver without
// the test itself taking 30+ seconds; production code must never change it.
var approvalBackstopMargin = 30 * time.Second

// Approver asks the human. It is an interface so that internal/mcpsrv does not
// depend on Telegram, and so the tests above need no network.
type Approver interface {
	Request(ctx context.Context, call ToolCall, d policy.Decision) (approved bool, latency time.Duration, err error)
}

type ToolCall struct {
	Actor string // "hermes"
	Name  string
	Args  map[string]any

	// ArgsError, if non-empty, means the caller's arguments did not parse into
	// an object at all. Invoke carries it into policy.Decide rather than
	// refusing before Decide runs, so an unparseable call still goes through
	// the one evaluation order and still gets an audit record.
	ArgsError string
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

// Registrations returns tool metadata with handlers stripped, sorted by name
// so that capabilities() and chhayactl policy explain describe the tool set in
// a stable order. Schema and Redact are cloned, not just the handlers nilled
// out: a caller mutating the slice it got back must not be able to reach the
// copy that record() reads on every later call.
func (g *Gateway) Registrations() []Registration {
	out := make([]Registration, 0, len(g.tools))
	for _, r := range g.tools {
		r.Handle, r.Validate = nil, nil
		r.Schema = bytes.Clone(r.Schema)
		r.Redact = slices.Clone(r.Redact)
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b Registration) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// Invoke is the only exported path to a tool handler. It always runs, in order:
// policy.Decide → dispatch-or-refuse → audit.Append. There is no branch that
// skips the first or the last.
func (g *Gateway) Invoke(ctx context.Context, call ToolCall) ToolResult {
	start := g.now()
	reg, known := g.tools[call.Name]

	req := policy.Request{
		Tool:      call.Name,
		Args:      call.Args,
		Known:     known,
		Kind:      reg.Kind,
		Tier:      reg.Tier,
		CapKey:    reg.CapKey,
		ArgsOK:    call.ArgsError == "",
		ArgsError: call.ArgsError,
	}
	// Consulted whenever a validator was supplied, not only for tier 1: a
	// registration that attaches one and never has it called is a trap.
	if known && reg.Validate != nil {
		if err := callValidator(reg.Validate, call.Args); err != nil {
			req.ValidatorErr = err.Error()
		} else {
			req.ValidatorOK = true
		}
	} else {
		req.ValidatorOK = true // no validator to consult
	}

	st := g.state.Snapshot(start)
	d := policy.Decide(req, st, g.cfg, start)

	switch d.Outcome {
	case policy.Deny:
		return g.refuse(call, reg, st, d, start, "")

	case policy.NeedApproval:
		g.record(call, reg, st, d, "needs_approval", d.RuleID, d.Reason, nil, audit.Result{Status: "pending"}, start, "")

		// The approver owns the primary timeout and writes its own `timeout`
		// record. This is a backstop for an approver that accepts the
		// request and never answers at all: deliberately longer, so the
		// normal path still produces the approver's own record rather than
		// this one.
		apCtx, cancel := context.WithTimeout(ctx, g.cfg.ApprovalTimeout+approvalBackstopMargin)
		defer cancel()
		approved, latency, err := g.approver.Request(apCtx, call, d)
		if err != nil {
			// The safety channel itself is down. Refuse loudly, and tell Hermes
			// in a way it can repeat to the user: the agent becomes the
			// out-of-band alarm precisely when the alarm is what is broken.
			msg := "refused: the approval channel is unreachable, so this cannot be approved. " +
				"Tell the user their approver bot is not responding."
			ans := &audit.Approval{Answer: "channel_failed", LatencyMS: latency.Milliseconds()}
			g.record(call, reg, st, d, "deny", "approval.unreachable", msg, ans,
				audit.Result{Status: "refused", Error: err.Error()}, start, "critical")
			return ToolResult{IsError: true, RuleID: "approval.unreachable", Error: msg}
		}
		if !approved {
			ans := &audit.Approval{Answer: "denied_by_user", LatencyMS: latency.Milliseconds()}
			g.record(call, reg, st, d, "deny", d.RuleID, d.Reason, ans, audit.Result{Status: "refused"}, start, "")
			return ToolResult{IsError: true, RuleID: d.RuleID, Error: "refused: you declined this"}
		}

		// The world may have changed while we waited for a human. Re-decide
		// rather than act on the decision taken before the question was
		// asked: /stop pressed during the approval window — the most likely
		// moment for the user to press it, since the card is what alarmed
		// them — must still stop the write.
		st2 := g.state.Snapshot(g.now())
		d2 := policy.Decide(req, st2, g.cfg, g.now())
		ans := &audit.Approval{Answer: "approved", LatencyMS: latency.Milliseconds()}
		if d2.Outcome == policy.Deny {
			g.record(call, reg, st2, d2, "deny", d2.RuleID, d2.Reason, ans, audit.Result{Status: "refused"}, start, "")
			return ToolResult{IsError: true, RuleID: d2.RuleID, Error: d2.Reason}
		}

		res := g.dispatch(ctx, reg, call, start)
		decision, ruleID, reason, result := recordedOutcome(d2, res)
		g.record(call, reg, st2, d2, decision, ruleID, reason, ans, result, start, "")
		return res

	default: // policy.Allow
		res := g.dispatch(ctx, reg, call, start)
		decision, ruleID, reason, result := recordedOutcome(d, res)
		g.record(call, reg, st, d, decision, ruleID, reason, nil, result, start, "")
		return res
	}
}

// recordedOutcome turns a dispatch result into the fields record() should
// log. A refusal manufactured by dispatch itself (its RuleID is non-empty) is
// a deny the gateway made on the way in, not an allow the tool failed at —
// logging it as "allow" would assert the gateway let through a call it
// actually blocked.
func recordedOutcome(d policy.Decision, res ToolResult) (decision, ruleID, reason string, result audit.Result) {
	if res.IsError && res.RuleID != "" {
		return "deny", res.RuleID, res.Error, audit.Result{Status: "refused"}
	}
	return "allow", d.RuleID, d.Reason, resultOf(res)
}

func (g *Gateway) dispatch(ctx context.Context, reg Registration, call ToolCall, start time.Time) ToolResult {
	if err := g.state.RecordCall(call.Name, start); err != nil {
		return ToolResult{IsError: true, RuleID: "state.unwritable",
			Error: "refused: the gateway cannot record this call, so it will not make it"}
	}

	// Taint is set before the handler runs, not after it returns. An
	// untrusted tool's error string is untrusted content too — a fetched
	// page's body, a remote server's message — and a handler can take a side
	// effect and then fail, so the flag has to be set on the way in, when
	// refusing is still possible, rather than on the way out.
	if reg.Trust == Untrusted {
		if err := g.state.SetTaint(true, reg.Name, g.now()); err != nil {
			return ToolResult{IsError: true, RuleID: "state.unwritable",
				Error: "refused: this tool returns untrusted content and the gateway could not mark the session, so it was not called"}
		}
	}

	v, err := callHandler(ctx, reg.Handle, call.Args)
	if err != nil {
		return ToolResult{Trust: reg.Trust, IsError: true, Error: err.Error()}
	}
	return ToolResult{Value: v, Trust: reg.Trust}
}

// callHandler and callValidator recover a panic into an error rather than
// letting it unwind past this package. A panicking handler becomes a failed
// ToolResult that dispatch's caller still records. A panicking validator
// becomes a ValidatorErr, which policy.Decide denies on for a tier-1 tool and
// otherwise ignores, same as any other validator failure for that tier —
// either way Invoke keeps running and still reaches record(), instead of the
// panic propagating straight past the audit log.

func callHandler(ctx context.Context, h Handler, args map[string]any) (v any, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("tool handler panicked: %v", r)
		}
	}()
	return h(ctx, args)
}

func callValidator(v Validator, args map[string]any) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("validator panicked: %v", r)
		}
	}()
	return v(args)
}

func (g *Gateway) refuse(call ToolCall, reg Registration, st policy.State, d policy.Decision, start time.Time, sev string) ToolResult {
	g.record(call, reg, st, d, "deny", d.RuleID, d.Reason, nil, audit.Result{Status: "refused"}, start, sev)
	return ToolResult{IsError: true, RuleID: d.RuleID, Error: d.Reason}
}

func (g *Gateway) record(call ToolCall, reg Registration, st policy.State, d policy.Decision,
	decision, ruleID, reason string, ap *audit.Approval, res audit.Result, start time.Time, severity string) {

	// MarshalArgs, not json.Marshal: the audit package owns its encoding mode
	// so that the bytes written are the bytes hashed. The redact paths are
	// copied into a fresh slice rather than appended onto reg.Redact
	// directly: if that slice ever has spare capacity, appending onto it in
	// place could race with a concurrent Invoke of the same tool; copying
	// first removes the question entirely.
	redactPaths := make([]string, 0, len(reg.Redact)+len(d.Redact))
	redactPaths = append(redactPaths, reg.Redact...)
	redactPaths = append(redactPaths, d.Redact...)
	args := audit.MarshalArgs(audit.Redact(call.Args, redactPaths))

	r := audit.Record{
		Actor: orDefault(call.Actor, "unknown"), Tool: call.Name,
		Kind: reg.Kind.String(), Tier: int(d.Tier),
		Args: args, Decision: decision, Rule: ruleID, Reason: reason,
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
