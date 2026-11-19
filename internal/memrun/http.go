package memrun

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// HTTPModel calls an OpenAI-compatible chat-completions endpoint: the one Hermes uses
// (https://api.anthropic.com/v1). The key is sent as a bearer token and never appears in an error.
type HTTPModel struct {
	Base, Key, Model string
	MaxTokens        int // reply limit; 0 means 1024
	Client           *http.Client
}

type wireCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type wireMsg struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []wireCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

func (h *HTTPModel) Chat(ctx context.Context, msgs []Message, tools []ToolDef) (Message, Usage, error) {
	// No temperature: this API rejects it for current models (HTTP 400, 2026-11-17).
	mt := h.MaxTokens
	if mt == 0 {
		mt = 1024
	}
	req := map[string]any{"model": h.Model, "max_tokens": mt}
	var wm []wireMsg
	for _, m := range msgs {
		w := wireMsg{Role: m.Role, Content: m.Content, ToolCallID: m.ToolCallID}
		for _, c := range m.ToolCalls {
			wc := wireCall{ID: c.ID, Type: "function"}
			wc.Function.Name, wc.Function.Arguments = c.Name, c.Args
			w.ToolCalls = append(w.ToolCalls, wc)
		}
		wm = append(wm, w)
	}
	req["messages"] = wm
	if len(tools) > 0 {
		var ts []map[string]any
		for _, t := range tools {
			ts = append(ts, map[string]any{"type": "function", "function": map[string]any{
				"name": t.Name, "description": t.Description, "parameters": json.RawMessage(t.Schema)}})
		}
		req["tools"] = ts
	}
	body, err := json.Marshal(req)
	if err != nil {
		return Message{}, Usage{}, err
	}
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(h.Base, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return Message{}, Usage{}, err
	}
	hr.Header.Set("Content-Type", "application/json")
	hr.Header.Set("Authorization", "Bearer "+h.Key)
	c := h.Client
	if c == nil {
		c = &http.Client{Timeout: 90 * time.Second}
	}
	resp, err := c.Do(hr)
	if err != nil {
		return Message{}, Usage{}, h.scrub(err.Error())
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return Message{}, Usage{}, h.scrub(err.Error())
	}
	if resp.StatusCode != http.StatusOK {
		msg := string(raw)
		if len(msg) > 200 {
			msg = msg[:200]
		}
		return Message{}, Usage{}, h.scrub(fmt.Sprintf("model call failed: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(msg)))
	}
	var out struct {
		Choices []struct {
			Message wireMsg `json:"message"`
		} `json:"choices"`
		Usage struct {
			Prompt     int `json:"prompt_tokens"`
			Completion int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || len(out.Choices) == 0 {
		return Message{}, Usage{}, fmt.Errorf("model call returned no choices")
	}
	r := out.Choices[0].Message
	msg := Message{Role: "assistant", Content: r.Content}
	for _, c := range r.ToolCalls {
		msg.ToolCalls = append(msg.ToolCalls, ToolCall{ID: c.ID, Name: c.Function.Name, Args: c.Function.Arguments})
	}
	return msg, Usage{In: out.Usage.Prompt, Out: out.Usage.Completion}, nil
}

// scrub returns an error whose text cannot contain the key, even if the server echoed it back.
func (h *HTTPModel) scrub(s string) error {
	if h.Key != "" {
		s = strings.ReplaceAll(s, h.Key, "[redacted]")
	}
	return fmt.Errorf("%s", s)
}
