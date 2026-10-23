// Package approval owns the approver bot: the only process holding the
// approver token. Hermes has no path to it, which is what makes approvals and
// the kill switch unforgeable.
package approval

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

type User struct {
	ID int64 `json:"id"`
}

type Chat struct {
	ID int64 `json:"id"`
}

type Message struct {
	ID   int64  `json:"message_id"`
	From User   `json:"from"`
	Chat Chat   `json:"chat"`
	Text string `json:"text"`
}

type Callback struct {
	ID      string   `json:"id"`
	From    User     `json:"from"`
	Data    string   `json:"data"`
	Message *Message `json:"message"`
}

type Update struct {
	ID       int64     `json:"update_id"`
	Message  *Message  `json:"message"`
	Callback *Callback `json:"callback_query"`
}

type Button struct {
	Text string
	Data string
}

// Telegram is the four calls we make. An interface so the bot tests need no
// network, and so no Telegram library enters a security component.
type Telegram interface {
	GetMe(ctx context.Context) (botID int64, username string, err error)
	GetChat(ctx context.Context, chatID int64) error
	SendMessage(ctx context.Context, chatID int64, text string, buttons [][]Button) (messageID int64, err error)
	// AttachKeyboard adds an inline keyboard to a message already sent. The
	// callback token is derived from the message id, which does not exist
	// until the message does — so the keyboard cannot go out with it.
	AttachKeyboard(ctx context.Context, chatID, messageID int64, buttons [][]Button) error
	AnswerCallback(ctx context.Context, callbackID, text string) error
	GetUpdates(ctx context.Context, offset int64, timeout time.Duration) ([]Update, error)
}

type httpTG struct {
	token  string
	client *http.Client
}

func NewHTTPTelegram(token string, client *http.Client) Telegram {
	if client == nil {
		client = &http.Client{Timeout: 70 * time.Second} // longer than the poll
	}
	return &httpTG{token: token, client: client}
}

// call never includes the token in an error, because errors get logged.
func (t *httpTG) call(ctx context.Context, method string, body any, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	endpoint := "https://api.telegram.org/bot" + t.token + "/" + method
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(b))
	if err != nil {
		return fmt.Errorf("telegram %s: build request failed", method)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := t.client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			return fmt.Errorf("telegram %s: %v", method, ue.Err) // ue.Err has no URL, so no token
		}
		return fmt.Errorf("telegram %s: request failed", method)
	}
	defer resp.Body.Close()

	var env struct {
		OK          bool            `json:"ok"`
		Description string          `json:"description"`
		Result      json.RawMessage `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return fmt.Errorf("telegram %s: unreadable response", method)
	}
	if !env.OK {
		return fmt.Errorf("telegram %s: %s", method, env.Description)
	}
	if out != nil {
		return json.Unmarshal(env.Result, out)
	}
	return nil
}

func (t *httpTG) GetMe(ctx context.Context) (int64, string, error) {
	var me struct {
		ID       int64  `json:"id"`
		Username string `json:"username"`
	}
	if err := t.call(ctx, "getMe", map[string]any{}, &me); err != nil {
		return 0, "", err
	}
	return me.ID, me.Username, nil
}

func (t *httpTG) GetChat(ctx context.Context, chatID int64) error {
	return t.call(ctx, "getChat", map[string]any{"chat_id": chatID}, nil)
}

func (t *httpTG) SendMessage(ctx context.Context, chatID int64, text string, buttons [][]Button) (int64, error) {
	body := map[string]any{"chat_id": chatID, "text": text, "parse_mode": "HTML"}
	if len(buttons) > 0 {
		body["reply_markup"] = map[string]any{"inline_keyboard": keyboard(buttons)}
	}
	var msg Message
	if err := t.call(ctx, "sendMessage", body, &msg); err != nil {
		return 0, err
	}
	return msg.ID, nil
}

func (t *httpTG) AttachKeyboard(ctx context.Context, chatID, messageID int64, buttons [][]Button) error {
	return t.call(ctx, "editMessageReplyMarkup", map[string]any{
		"chat_id": chatID, "message_id": messageID,
		"reply_markup": map[string]any{"inline_keyboard": keyboard(buttons)},
	}, nil)
}

func (t *httpTG) AnswerCallback(ctx context.Context, id, text string) error {
	return t.call(ctx, "answerCallbackQuery", map[string]any{"callback_query_id": id, "text": text}, nil)
}

func keyboard(buttons [][]Button) [][]map[string]string {
	rows := make([][]map[string]string, 0, len(buttons))
	for _, row := range buttons {
		r := make([]map[string]string, 0, len(row))
		for _, b := range row {
			r = append(r, map[string]string{"text": b.Text, "callback_data": b.Data})
		}
		rows = append(rows, r)
	}
	return rows
}

func (t *httpTG) GetUpdates(ctx context.Context, offset int64, timeout time.Duration) ([]Update, error) {
	var ups []Update
	// allowed_updates must be a JSON array, not a comma string. Getting this
	// wrong during the spike produced "no callback" while three taps sat in
	// the queue — the error response parsed as an empty result.
	body := map[string]any{
		"offset":          offset,
		"timeout":         int(timeout.Seconds()),
		"allowed_updates": []string{"message", "callback_query"},
	}
	if err := t.call(ctx, "getUpdates", body, &ups); err != nil {
		return nil, err
	}
	return ups, nil
}
