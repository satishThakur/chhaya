package cost

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

// Session is one Hermes session row.
type Session struct {
	ID         string
	Source     string
	Model      string
	Started    time.Time
	LastActive time.Time
	Ended      bool
	Messages   int64
	ToolCalls  int64
	Usage      Usage
}

// ParseSessions reads the JSON `sqlite3 -json` prints for Query.
func ParseSessions(b []byte) ([]Session, error) {
	if len(bytes.TrimSpace(b)) == 0 {
		return nil, nil
	}
	var rows []struct {
		ID           string   `json:"id"`
		Source       *string  `json:"source"`
		Model        *string  `json:"model"`
		Started      float64  `json:"started_at"`
		Ended        *float64 `json:"ended_at"`
		LastActivity *float64 `json:"last_activity_at"`
		LastMsg      *float64 `json:"last_msg"`
		Messages     *int64   `json:"message_count"`
		ToolCalls    *int64   `json:"tool_call_count"`
		InputTokens  *int64   `json:"input_tokens"`
		OutputTokens *int64   `json:"output_tokens"`
		CacheRead    *int64   `json:"cache_read_tokens"`
		CacheWrite   *int64   `json:"cache_write_tokens"`
		APICalls     *int64   `json:"api_call_count"`
	}
	if err := json.Unmarshal(b, &rows); err != nil {
		return nil, fmt.Errorf("sessions: %w", err)
	}
	n := func(p *int64) int64 {
		if p == nil {
			return 0
		}
		return *p
	}
	sec := func(f float64) time.Time { return time.Unix(int64(f), 0) }
	var out []Session
	for _, r := range rows {
		s := Session{
			ID: r.ID, Started: sec(r.Started), Ended: r.Ended != nil,
			Messages: n(r.Messages), ToolCalls: n(r.ToolCalls),
			Usage: Usage{Input: n(r.InputTokens), Output: n(r.OutputTokens), CacheRead: n(r.CacheRead), CacheWrite: n(r.CacheWrite), APICalls: n(r.APICalls)},
		}
		if r.Source != nil {
			s.Source = *r.Source
		}
		if r.Model != nil {
			s.Model = *r.Model
		}
		// The newest sign of life: the last activity stamp, or the last message, or the start.
		s.LastActive = s.Started
		for _, f := range []*float64{r.LastActivity, r.LastMsg, r.Ended} {
			if f != nil && sec(*f).After(s.LastActive) {
				s.LastActive = sec(*f)
			}
		}
		out = append(out, s)
	}
	return out, nil
}

// Query is the SQL that feeds ParseSessions: sessions that used any tokens at all.
const Query = `select s.id, s.source, s.model, s.started_at, s.ended_at, s.last_activity_at,
  (select max(m.timestamp) from messages m where m.session_id = s.id) as last_msg,
  s.message_count, s.tool_call_count, s.input_tokens, s.output_tokens, s.cache_read_tokens,
  s.cache_write_tokens, s.api_call_count
from sessions s where coalesce(s.input_tokens,0) + coalesce(s.output_tokens,0) > 0
order by s.started_at`

// State remembers what was already reported, so a session is announced once, and again only
// when it grew afterwards (then as the difference).
type State map[string]Reported

type Reported struct {
	Tokens int64   `json:"tokens"`
	USD    float64 `json:"usd"`
}

func LoadState(path string) (State, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return State{}, nil
	}
	if err != nil {
		return nil, err
	}
	s := State{}
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("%s: %w (not overwritten)", path, err)
	}
	return s, nil
}

func (s State) Save(path string) error {
	b, _ := json.MarshalIndent(s, "", " ")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Sender is the slice of the approver bot this needs.
type Sender interface {
	Send(ctx context.Context, text string) error
}

type Options struct {
	Prices Prices
	State  State
	Now    time.Time
	// Idle is how long a session must be quiet to count as finished when Hermes has not ended
	// it. Match session_reset.idle_minutes plus slack. Since drops anything quieter for longer
	// than this, so the first run on a box does not announce its whole history.
	Idle, Since time.Duration
	Zone        *time.Location
}

type Line struct {
	S        Session
	R        Result
	Continue bool    // already reported; this is the growth since
	Delta    float64 // USD to announce
}

// Due returns the finished sessions with something new to say, oldest first.
func Due(ss []Session, o Options) []Line {
	var out []Line
	for _, s := range ss {
		quiet := o.Now.Sub(s.LastActive)
		if !s.Ended && quiet < o.Idle {
			continue
		}
		if quiet > o.Since {
			continue
		}
		tokens := s.Usage.Input + s.Usage.Output
		prev, seen := o.State[s.ID]
		if seen && tokens <= prev.Tokens {
			continue
		}
		r := o.Prices.Cost(s.Model, s.Usage)
		out = append(out, Line{S: s, R: r, Continue: seen, Delta: r.USD - prev.USD})
	}
	return out
}

// Today sums every session that started on o.Now's calendar day in o.Zone.
func Today(ss []Session, o Options) (usd float64, n int, partial bool) {
	y, m, d := o.Now.In(o.Zone).Date()
	for _, s := range ss {
		sy, sm, sd := s.Started.In(o.Zone).Date()
		if sy != y || sm != m || sd != d {
			continue
		}
		r := o.Prices.Cost(s.Model, s.Usage)
		usd += r.USD
		n++
		partial = partial || !r.Complete
	}
	return
}

func short(id string) string {
	if len(id) > 24 {
		return id[:24] + "…"
	}
	return id
}

func commas(n int64) string {
	s := fmt.Sprint(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// Message is the Telegram text for one session (Telegram HTML; everything dynamic is escaped).
func Message(l Line, todayUSD float64, todayN int, todayPartial bool) string {
	var b strings.Builder
	head := fmt.Sprintf("Session cost: <b>$%.3f</b>", l.R.USD)
	if l.Continue {
		head = fmt.Sprintf("Session cost: <b>+$%.3f</b> since last report (total $%.3f)", l.Delta, l.R.USD)
	}
	if !l.R.Complete {
		head += " (partial)"
	}
	fmt.Fprintln(&b, head)
	fmt.Fprintf(&b, "%s · %s · %s\n", html.EscapeString(l.S.Source), html.EscapeString(l.S.Model), html.EscapeString(short(l.S.ID)))
	fmt.Fprintf(&b, "%d model calls · %d messages · %d tool calls\n", l.S.Usage.APICalls, l.S.Messages, l.S.ToolCalls)
	fmt.Fprintf(&b, "in %s · out %s · cache read %s\n", commas(l.S.Usage.Input), commas(l.S.Usage.Output), commas(l.S.Usage.CacheRead))
	for _, n := range l.R.Notes {
		fmt.Fprintf(&b, "note: %s\n", html.EscapeString(n))
	}
	p := ""
	if todayPartial {
		p = " (some not priced)"
	}
	fmt.Fprintf(&b, "Today: $%.2f over %d sessions%s", todayUSD, todayN, p)
	return b.String()
}

// Run sends one message per due session and records each only after Telegram accepted it, so
// a failed send is retried by the next run. It returns what it sent.
func Run(ctx context.Context, ss []Session, o Options, send Sender, statePath string) ([]Line, error) {
	due := Due(ss, o)
	usd, n, partial := Today(ss, o)
	var sent []Line
	for _, l := range due {
		if err := send.Send(ctx, Message(l, usd, n, partial)); err != nil {
			return sent, fmt.Errorf("session %s: %w", l.S.ID, err)
		}
		o.State[l.S.ID] = Reported{Tokens: l.S.Usage.Input + l.S.Usage.Output, USD: l.R.USD}
		if err := o.State.Save(statePath); err != nil {
			return sent, fmt.Errorf("sent %s but could not record it: %w", l.S.ID, err)
		}
		sent = append(sent, l)
	}
	return sent, nil
}

// Table lists every session by cost, dearest first: the measurement view.
func Table(w io.Writer, ss []Session, o Options) {
	type row struct {
		s Session
		r Result
	}
	var rows []row
	var total float64
	for _, s := range ss {
		r := o.Prices.Cost(s.Model, s.Usage)
		rows = append(rows, row{s, r})
		total += r.USD
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].r.USD > rows[j].r.USD })
	fmt.Fprintf(w, "%-9s %-9s %-18s %-9s %6s %11s %9s  %s\n", "USD", "source", "model", "day", "calls", "in", "out", "session")
	for _, x := range rows {
		mark := ""
		if !x.r.Complete {
			mark = " *"
		}
		fmt.Fprintf(w, "%-9s %-9s %-18s %-9s %6d %11s %9s  %s%s\n", fmt.Sprintf("$%.3f", x.r.USD), x.s.Source, x.s.Model,
			x.s.Started.In(o.Zone).Format("01-02"), x.s.Usage.APICalls, commas(x.s.Usage.Input), commas(x.s.Usage.Output), short(x.s.ID), mark)
	}
	fmt.Fprintf(w, "total $%.2f over %d sessions   (* = partly or not priced)\n", total, len(rows))
}

func StatePath(dataDir string) string { return filepath.Join(dataDir, "cost-reported.json") }
