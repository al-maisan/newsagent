// Package telegram sends messages through the Telegram Bot API.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// BaseURL is the Telegram Bot API endpoint.
const BaseURL = "https://api.telegram.org"

// MaxLen is Telegram's message length limit.
const MaxLen = 4096

// Client sends HTML-formatted messages to one chat.
type Client struct {
	HTTP    *http.Client
	BaseURL string
	Token   string
	ChatID  string
}

// Send posts one message (at most MaxLen characters).
func (c *Client) Send(ctx context.Context, html string) error {
	body, err := json.Marshal(map[string]any{
		"chat_id":              c.ChatID,
		"text":                 html,
		"parse_mode":           "HTML",
		"link_preview_options": map[string]bool{"is_disabled": true},
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/bot"+c.Token+"/sendMessage", bytes.NewReader(body))
	if err != nil {
		return errors.New("telegram: build request failed") // the URL holds the token
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		// *url.Error prints the URL, which contains the bot token.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("telegram: %w", err)
	}
	defer resp.Body.Close()
	var r struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err := json.Unmarshal(raw, &r); err != nil {
		return fmt.Errorf("telegram: http %d: bad response", resp.StatusCode)
	}
	if !r.OK {
		return &APIError{Code: resp.StatusCode, Description: r.Description}
	}
	return nil
}

// APIError is an error response from the Bot API.
type APIError struct {
	Code        int
	Description string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("telegram: http %d: %s", e.Code, e.Description)
}

// rejections are Bot API errors caused by the message itself.
var rejections = []string{"can't parse entities", "message is too long", "message text is empty", "text must be non-empty"}

// IsRejected reports whether Telegram refused the message because of its
// content, so sending the same message again can never succeed.
func IsRejected(err error) bool {
	var e *APIError
	if !errors.As(err, &e) || e.Code != http.StatusBadRequest {
		return false
	}
	d := strings.ToLower(e.Description)
	for _, r := range rejections {
		if strings.Contains(d, r) {
			return true
		}
	}
	return false
}

// Split cuts text into chunks of at most limit bytes at line boundaries.
// Lines longer than limit are cut hard; callers keep lines short.
func Split(text string, limit int) []string {
	var out []string
	var cur strings.Builder
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		for len(line) > limit {
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
			out = append(out, line[:limit])
			line = line[limit:]
		}
		if cur.Len() > 0 && cur.Len()+1+len(line) > limit {
			out = append(out, cur.String())
			cur.Reset()
		}
		if cur.Len() > 0 {
			cur.WriteByte('\n')
		}
		cur.WriteString(line)
	}
	if strings.TrimSpace(cur.String()) != "" {
		out = append(out, cur.String())
	}
	return out
}
