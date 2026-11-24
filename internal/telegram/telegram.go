// Package telegram sends messages through the Telegram Bot API.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf16"
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

// Send posts one HTML message (at most MaxLen characters). If Telegram
// rejects it, Send retries once as plain text (see PlainText). It returns a
// rejection only if the plain text was rejected too; any other failure of
// the retry is returned as is, so the caller retries later.
func (c *Client) Send(ctx context.Context, html string) error {
	err := c.send(ctx, html, "HTML")
	if !IsRejected(err) {
		return err
	}
	if perr := c.send(ctx, PlainText(html), ""); perr != nil {
		if IsRejected(perr) {
			return err
		}
		return fmt.Errorf("plain-text resend after %v: %w", err, perr)
	}
	slog.Warn("telegram rejected HTML, sent as plain text", "err", err)
	return nil
}

// maxRetryAfter is the longest rate-limit wait Send sits out; a longer one
// fails, and the caller retries on its next run.
const maxRetryAfter = 60 * time.Second

// send posts once and, if Telegram rate-limits it (429 with retry_after),
// waits as asked and posts once more.
func (c *Client) send(ctx context.Context, text, parseMode string) error {
	err := c.post(ctx, text, parseMode)
	var e *APIError
	if !errors.As(err, &e) || e.Code != http.StatusTooManyRequests || e.RetryAfter <= 0 {
		return err
	}
	wait := time.Duration(e.RetryAfter) * time.Second
	if wait > maxRetryAfter {
		return err
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return err
	case <-t.C:
	}
	return c.post(ctx, text, parseMode)
}

func (c *Client) post(ctx context.Context, text, parseMode string) error {
	msg := map[string]any{
		"chat_id":              c.ChatID,
		"text":                 text,
		"link_preview_options": map[string]bool{"is_disabled": true},
	}
	if parseMode != "" {
		msg["parse_mode"] = parseMode
	}
	body, err := json.Marshal(msg)
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
		Parameters  struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err := json.Unmarshal(raw, &r); err != nil {
		return fmt.Errorf("telegram: http %d: bad response", resp.StatusCode)
	}
	if !r.OK {
		return &APIError{Code: resp.StatusCode, Description: r.Description, RetryAfter: r.Parameters.RetryAfter}
	}
	return nil
}

// APIError is an error response from the Bot API.
type APIError struct {
	Code        int
	Description string
	RetryAfter  int // seconds, set on 429
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

var (
	anchorRe = regexp.MustCompile(`<a href="([^"]*)">(.*?)</a>`)
	tagRe    = regexp.MustCompile(`<[^>]*>`)
)

// PlainText turns a message from this program's HTML into plain text:
// links become "label url", other tags are dropped, entities unescaped, and
// the result is cut to MaxLen UTF-16 units (Telegram's length unit).
func PlainText(h string) string {
	s := html.UnescapeString(tagRe.ReplaceAllString(anchorRe.ReplaceAllString(h, "$2 $1"), ""))
	n := 0
	for i, r := range s {
		if n += utf16.RuneLen(r); n > MaxLen-1 {
			return s[:i] + "…"
		}
	}
	return s
}
