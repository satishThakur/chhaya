// Package cost turns the token counts Hermes keeps per session into dollars, and picks the
// finished sessions that have not been reported yet. Hermes itself records the tokens but not
// the cost (cost_status is "unknown" against a direct provider), so the price table is ours.
package cost

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Price is USD per million tokens. A nil cache price means "not published, not guessed".
type Price struct {
	Input      float64  `yaml:"input"`
	Output     float64  `yaml:"output"`
	CacheRead  *float64 `yaml:"cache_read"`
	CacheWrite *float64 `yaml:"cache_write"`

	LongPromptTokens int64   `yaml:"long_prompt_tokens"`
	LongInput        float64 `yaml:"long_input"`
	LongOutput       float64 `yaml:"long_output"`
}

type Prices map[string]Price

// LoadPrices reads config/pricing.yaml. An empty or unreadable file is an error: with no table
// every session would silently cost nothing.
func LoadPrices(path string) (Prices, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f struct {
		Models Prices `yaml:"models"`
	}
	dec := yaml.NewDecoder(bytesReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(f.Models) == 0 {
		return nil, fmt.Errorf("%s: no models", path)
	}
	for name, p := range f.Models {
		if p.Input <= 0 || p.Output <= 0 {
			return nil, fmt.Errorf("%s: model %q needs a positive input and output price", path, name)
		}
	}
	return f.Models, nil
}

// Usage is what Hermes recorded for a session.
type Usage struct {
	Input, Output, CacheRead, CacheWrite int64
	APICalls                             int64
}

// Result is a session's cost. Complete is false when something could not be priced; USD then
// covers only what could, and Notes say what is missing.
type Result struct {
	USD      float64
	Complete bool
	Notes    []string
}

func (p Prices) Cost(model string, u Usage) Result {
	pr, ok := p[model]
	if !ok {
		return Result{Notes: []string{fmt.Sprintf("no price for model %q in config/pricing.yaml", model)}}
	}
	r := Result{Complete: true}
	in, out := pr.Input, pr.Output
	if pr.LongPromptTokens > 0 && u.APICalls > 0 {
		if avg := (u.Input + u.CacheRead + u.CacheWrite) / u.APICalls; avg > pr.LongPromptTokens {
			in, out = pr.LongInput, pr.LongOutput
			r.Notes = append(r.Notes, fmt.Sprintf("estimate: average prompt %dk per call is over %dk, long-prompt rates applied", avg/1000, pr.LongPromptTokens/1000))
		}
	}
	r.USD = float64(u.Input)*in/1e6 + float64(u.Output)*out/1e6
	for _, c := range []struct {
		name   string
		tokens int64
		price  *float64
	}{{"cache read", u.CacheRead, pr.CacheRead}, {"cache write", u.CacheWrite, pr.CacheWrite}} {
		switch {
		case c.tokens == 0:
		case c.price == nil:
			r.Complete = false
			r.Notes = append(r.Notes, fmt.Sprintf("%d %s tokens not priced (no %s price for %s)", c.tokens, c.name, c.name, model))
		default:
			r.USD += float64(c.tokens) * *c.price / 1e6
		}
	}
	return r
}
