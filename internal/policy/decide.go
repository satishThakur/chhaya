package policy

import (
	"fmt"
	"time"
)

// Decide applies the evaluation order from the M0b spec, §4.2. The order is
// the security property: each rule can only ever make the answer stricter than
// the rules below it, so reading top to bottom tells you what cannot happen.
func Decide(req Request, st State, cfg Config, now time.Time) Decision {
	d := Decision{Tier: req.Tier}

	// 1. Unknown tool. First, so that nothing below can be reached by a name
	//    the registry never approved.
	if !req.Known {
		return deny(d, "tool.unknown", fmt.Sprintf("blocked: %q is not a registered tool", req.Tool))
	}

	// 2. Kill switch, writes only. Reads stay open deliberately: a gateway that
	//    cannot answer capabilities() while stopped cannot explain why it is stopped.
	if st.KillSwitch && req.Kind == Write {
		return deny(d, "killswitch.active",
			fmt.Sprintf("blocked: kill switch active since %s; /resume on the approver bot to lift it",
				st.KilledAt.UTC().Format(time.RFC3339)))
	}

	// 3. Tier 3 is never exposed. Registering one is a bug; this is the backstop.
	if req.Tier == TierIrreversible {
		return deny(d, "tier3.not_exposed", "blocked: irreversible actions are not exposed through this gateway")
	}

	// 4. Taint escalates any write to approval, whatever its tier.
	if st.Tainted && req.Kind == Write {
		d.Outcome = NeedApproval
		d.RuleID = "taint.write"
		d.Reason = fmt.Sprintf("approval required: untrusted content from %s entered the session at %s; /clear on the approver bot to reset",
			st.TaintSource, st.TaintedAt.UTC().Format(time.RFC3339))
		return withCaps(d, req, st, cfg)
	}

	// 5. Tier resolution.
	switch req.Tier {
	case TierInternal:
		d.Outcome, d.RuleID, d.Reason = Allow, "tier0.allow", "allowed: internal action"
	case TierReversible:
		if !req.ValidatorOK {
			msg := req.ValidatorErr
			if msg == "" {
				msg = "validator rejected the arguments"
			}
			return deny(d, "validator.failed", "blocked: "+msg)
		}
		d.Outcome, d.RuleID, d.Reason = Allow, "tier1.validated", "allowed: reversible action, validators passed"
	case TierMoney:
		d.Outcome, d.RuleID, d.Reason = NeedApproval, "tier2.approval", "approval required: this spends money or sends data outside"
	default:
		// Catches any tier the gateway does not recognise. Denying is the
		// fail-closed answer.
		return deny(d, "tier.unknown", fmt.Sprintf("blocked: tier %d is not a tier this gateway knows", req.Tier))
	}

	// 6. Caps and rate limits. They can only tighten what step 5 decided.
	return withCaps(d, req, st, cfg)
}

func withCaps(d Decision, req Request, st State, cfg Config) Decision {
	if req.CapKey != "" {
		limit, ok := cfg.Caps[req.CapKey]
		if !ok {
			return deny(d, "cap.unknown",
				fmt.Sprintf("blocked: no spending cap is configured for %q, which reads as zero", req.CapKey))
		}
		if req.AmountMinor > limit.PerOrderMinor {
			return deny(d, "cap."+req.CapKey+".per_order",
				fmt.Sprintf("blocked: %s is over the per-order cap of %s", rupees(req.AmountMinor), rupees(limit.PerOrderMinor)))
		}
		if st.Spent[req.CapKey]+req.AmountMinor > limit.PerDayMinor {
			left := limit.PerDayMinor - st.Spent[req.CapKey]
			if left < 0 {
				left = 0
			}
			return deny(d, "cap."+req.CapKey+".daily",
				fmt.Sprintf("blocked: over the daily cap; %s left today", rupees(left)))
		}
	}

	if cfg.DefaultRatePerHour <= 0 {
		return deny(d, "ratelimit.unset",
			"blocked: no rate limit is configured, which reads as zero calls per hour")
	}
	if st.Calls[req.Tool] >= cfg.DefaultRatePerHour {
		return deny(d, "ratelimit.tool",
			fmt.Sprintf("blocked: %s has been called %d times this hour, the limit is %d",
				req.Tool, st.Calls[req.Tool], cfg.DefaultRatePerHour))
	}

	return d
}

func deny(d Decision, rule, reason string) Decision {
	d.Outcome, d.RuleID, d.Reason = Deny, rule, reason
	return d
}

// rupees renders minor units for a human reading an approval card.
func rupees(minor int64) string {
	if minor%100 == 0 {
		return fmt.Sprintf("₹%d", minor/100)
	}
	return fmt.Sprintf("₹%d.%02d", minor/100, minor%100)
}
