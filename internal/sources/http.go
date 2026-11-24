// Package sources fetches news items from Hacker News, RSS/Atom feeds and X.
package sources

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	userAgent    = "newsagent/1.0 (+https://github.com/al-maisan/newsagent)"
	maxBodyBytes = 10 << 20
	maxTextRunes = 400
)

// get fetches url and returns the body; non-2xx responses are errors.
func get(ctx context.Context, c *http.Client, url string, hdr http.Header) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		return nil, &HTTPError{Status: resp.StatusCode, Body: clip(string(body), 200)}
	}
	return body, nil
}

// HTTPError is a non-2xx response.
type HTTPError struct {
	Status int
	Body   string // clipped
}

func (e *HTTPError) Error() string { return fmt.Sprintf("http %d: %s", e.Status, e.Body) }

func getJSON(ctx context.Context, c *http.Client, url string, hdr http.Header, v any) error {
	body, err := get(ctx, c, url, hdr)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("decode json: %w", err)
	}
	return nil
}

var (
	tagRe   = regexp.MustCompile(`<[^>]*>`)
	spaceRe = regexp.MustCompile(`[\s\p{Zs}]+`)
)

// plain strips HTML tags, unescapes entities and collapses whitespace.
func plain(s string) string {
	s = html.UnescapeString(tagRe.ReplaceAllString(s, " "))
	return strings.TrimSpace(spaceRe.ReplaceAllString(s, " "))
}

// clip shortens s to at most n runes.
func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "…"
}

// isWebURL reports whether u is an http(s) URL; other schemes are dropped.
func isWebURL(u string) bool {
	return strings.HasPrefix(u, "https://") || strings.HasPrefix(u, "http://")
}
