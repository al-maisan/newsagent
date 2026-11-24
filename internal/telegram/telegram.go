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
	"unicode/utf8"
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
	if err := json.Unmarshal(raw, &r); err != nil || !r.OK {
		return fmt.Errorf("telegram: http %d: %s", resp.StatusCode, r.Description)
	}
	return nil
}

// Split cuts text into chunks of at most limit bytes at line boundaries.
// Lines longer than limit are cut by cut, so each chunk stays valid HTML.
func Split(text string, limit int) []string {
	var out []string
	var cur strings.Builder
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		for len(line) > limit {
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
			n := cut(line, limit)
			out = append(out, line[:n])
			line = strings.TrimLeft(line[n:], " ")
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

// cut returns where to cut s so that s[:n] is at most limit bytes. It prefers
// the last space, then the last other point that is outside any tag, element
// or entity and on a rune boundary. If there is none, it cuts at a rune
// boundary (Telegram may then reject the part).
func cut(s string, limit int) int {
	best, space := 0, 0
	depth, inTag, closing, inEnt := 0, false, false, false
	for i := 0; i < len(s) && i <= limit; i++ {
		if i > 0 && depth == 0 && !inTag && !inEnt && utf8.RuneStart(s[i]) {
			best = i
			if s[i] == ' ' {
				space = i
			}
		}
		switch c := s[i]; {
		case inTag:
			if c == '>' {
				inTag = false
				if closing {
					depth--
				} else {
					depth++
				}
			}
		case inEnt:
			inEnt = c != ';'
		case c == '<':
			inTag, closing = true, i+1 < len(s) && s[i+1] == '/'
		case c == '&':
			inEnt = true
		}
	}
	switch {
	case space > 0:
		return space
	case best > 0:
		return best
	}
	n := limit
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	if n == 0 {
		return limit
	}
	return n
}
