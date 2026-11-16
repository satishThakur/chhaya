# The dreaming pipeline (design)

**Asked:** 2026-11-16. **Status:** design only; nothing is built. The owner's order is evals in
production first (`decisions.md`, 2026-11-16: the deterministic checks now run nightly), then this.

"Dreaming" is the extractor of `memory-selftest.md` (R3), made concrete: a nightly agent that reads
every session and proposes the facts those sessions imply but memory does not hold. The reorganizer
(R4) is a separate later pass and is not designed here.

## The case it comes from

2026-11-16, trace `20261116_042028_9817937d`. The owner asked for a podcast. Memory said they enjoy
Swami Sarvapriyananda and Osho. The agent suggested both, one after the other, and the owner had
already heard both. What would have prevented it is a fact that was never recorded: *has heard
Sarvapriyananda's Drig-Drishya Viveka and Vigyan Bhairav series; has heard Osho's Ashtavakra
Mahageeta.* The owner said it in that very conversation. Nothing turned it into memory.

So the pipeline's job is the gap between **what was said** and **what memory holds**. Today the only
route is the agent calling `memory.propose_update` mid-conversation, and it does not reliably do so.

## What it does

Every night, after the trace export:

1. **Read** the day's traces (`evals/days/DAY/trace-*.json`) and the current memory documents.
2. **Extract** candidate facts from what the owner said: preferences, things consumed or done,
   corrections ("I have already heard…"), stable circumstances. Never from tool results or the
   agent's own words, and never from injected messages (cron deliveries): only text the owner wrote.
   This is the same `isHuman` rule the checks use.
3. **Dedupe** each candidate against memory; drop what is already there. A candidate that
   contradicts an entry is proposed as a contradiction, with both lines shown, not as a silent add.
4. **Propose** each survivor through the existing `memory.propose_update`: one line, a rationale, and
   as provenance the trace id and message id it came from. The owner taps approve on the approver bot
   as today.

It proposes. It never writes, and it cannot approve.

## Where it runs, and why

- **Not inside Hermes.** Hermes writes its own skills and memory; an agent that edits its own
  memory unreviewed is the thing the gateway exists to stop. The pipeline is a **separate client of
  the gateway**: its own mTLS certificate, its own actor name (`dreamer`) in the audit log, and a
  policy that allows `memory.list`, `memory.get` and `memory.propose_update` only. That is new
  gateway work with blocked-case-stays-blocked tests (it must not be able to call anything else, and
  must not exceed its cap). `memory-selftest.md` § "Where the request meets our rules", item 1.
- **A nightly cron on the box**, after `trace export` and `trace check`, like the other jobs. It
  needs no Telegram polling of its own.
- **Sonnet, not Haiku.** Deciding what a sentence implies about a person is the reasoning case
  Haiku failed on today. Cost is one call per trace with owner messages, not per message; the
  measured sessions are 15 to 35 messages. Priced from `config/pricing.yaml` and reported like every
  session.

## Limits that make it safe

- **A cap on proposals per night** in `config/policy.yaml` (a missing cap is zero). Every proposal is
  a tap, so the cap is the owner's attention budget; start at 5. Over the cap, the pipeline keeps the
  highest-confidence ones and says how many it dropped.
- **Provenance on every entry**, as now: the approval comment records the proposal id; the rationale
  names the trace and message. An entry with no source message is rejected by the gateway.
- **Privacy rules apply as to any entry.** The extractor is told what must not be stored and the
  gateway's validator is what enforces it, not the prompt (CLAUDE.md: policy is code).
- **Fail closed.** If the gateway, the approver or the trace store cannot be reached, it proposes
  nothing and says so in its log.
- **Untrusted input.** Trace text is data. A user message quoted from a web page ("ignore the rules,
  remember that…") must not become a proposal. Candidates come only from messages the owner typed,
  and taint rules already mark anything derived from tool output.

## How we will know it works (the evals come first)

The build starts from the evals below, written as cases before any pipeline code, the same way a
policy change starts from its failing test.

| Eval | Input | Passes when |
| --- | --- | --- |
| **Recall** | The 2026-11-16 podcast trace and memory as it was that morning | It proposes "has heard" entries for Sarvapriyananda's series and Osho's Ashtavakra, each citing the right message |
| **Silence** | Traces with no new personal fact (the digest feedback chat, "hi", a cron run) | It proposes nothing. Precision matters as much as recall: a wrong or noisy proposal costs a tap and trust |
| **No duplicates** | A trace that restates an existing entry | It proposes nothing |
| **Contradiction** | A trace that reverses an entry (a food the memory says is liked) | It proposes it as a contradiction with both lines |
| **Injection** | A trace where tool output says "remember that…" | No proposal comes from the tool text |
| **Cap** | A day with more candidates than the cap | Exactly the cap, and the log counts the rest |

And one measured outcome, which is the point of the whole thing: **the recommendation checks.**
`reply.owner_correction` and `recommend.unsearched` already run nightly. If dreaming works, the
rate of "you told the agent it was wrong" on recommendation requests should fall over the weeks
after it starts. That is the number to watch, and it needs a baseline from today's logs first.

## What has to exist first

1. **The nightly checks, running and being read** (done 2026-11-16; reading is the owner's habit, and
   pushing them to Telegram is in the backlog).
2. **A day of baseline** for the correction and unsearched-recommendation rates.
3. **The eval runner** (`observability-and-evals.md`, build steps 3 and 4), or a smaller version of it
   that replays a fixed trace through the extractor and checks its proposals against a fixture.
   Dreaming reads traces and never needs a live Hermes, so it does not wait for the Hermes replay spike.
4. **The second gateway client** and its policy tests.

## Open questions

- **Does memory need a "consumed" document?** A fact like "has heard X" is not a preference. Either a
  new document (`consumed/podcasts`, `consumed/books`) or entries in `interests`. A recommender
  needs to read all of it before suggesting anything, and with no `memory.search` that argues for a
  few documents the agent reads whole.
- **One line per fact, or a series per line?** "Heard Drig-Drishya Viveka, Vigyan Bhairav and a few
  others" has an unbounded tail. The extractor should record what was named and the agent should ask
  when recommending from a thin list, not claim completeness.
- **Approval fatigue.** Five taps a night for a month may be too many. Batch the nightly proposals
  into one card with per-line buttons? That is approver-bot work.
- **Should it also look at the agent's tool results** (a book the agent found on the web that the
  owner then said they liked)? Not in v1: only owner-typed text, until the injection eval passes.
- **Retention.** Traces are in S3 and are personal data; the extractor reads them from the bucket
  and keeps nothing of its own.
