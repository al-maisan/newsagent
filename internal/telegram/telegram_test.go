package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
	"unicode/utf8"
)

func TestSend(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/botTOKEN/sendMessage" {
			http.Error(w, `{"ok":false,"description":"bad path"}`, 404)
			return
		}
		json.NewDecoder(r.Body).Decode(&got)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), BaseURL: srv.URL, Token: "TOKEN", ChatID: "123"}
	if err := c.Send(context.Background(), "<b>hi</b>"); err != nil {
		t.Fatal(err)
	}
	if got["chat_id"] != "123" || got["parse_mode"] != "HTML" || got["text"] != "<b>hi</b>" {
		t.Fatalf("bad request %v", got)
	}
}

func TestSendAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		w.Write([]byte(`{"ok":false,"description":"Bad Request: can't parse entities"}`))
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), BaseURL: srv.URL, Token: "TOKEN", ChatID: "1"}
	err := c.Send(context.Background(), "x")
	if err == nil || !strings.Contains(err.Error(), "can't parse entities") {
		t.Fatalf("want API error, got %v", err)
	}
}

func TestSendNetworkErrorHidesToken(t *testing.T) {
	c := &Client{HTTP: http.DefaultClient, BaseURL: "http://127.0.0.1:1", Token: "SECRET123", ChatID: "1"}
	err := c.Send(context.Background(), "x")
	if err == nil || strings.Contains(err.Error(), "SECRET123") {
		t.Fatalf("error must not contain token: %v", err)
	}
}

func TestSplit(t *testing.T) {
	text := strings.Repeat("a", 6) + "\n" + strings.Repeat("b", 6) + "\n" + strings.Repeat("c", 25)
	parts := Split(text, 10)
	want := []string{"aaaaaa", "bbbbbb", "cccccccccc", "cccccccccc", "ccccc"}
	if strings.Join(parts, "|") != strings.Join(want, "|") {
		t.Fatalf("got %q", parts)
	}
	if got := Split("a\nb", 10); len(got) != 1 || got[0] != "a\nb" {
		t.Fatalf("got %q", got)
	}
}

func TestSplitKeepsHTMLAndRunesWhole(t *testing.T) {
	link := `<a href="https://e.com/?a=1&amp;b=2">HN</a>`
	line := "• " + strings.Repeat("é", 30) + " " + link + " " + link + " " + link
	for limit := len(link); limit <= len(line); limit++ {
		parts := Split(line, limit)
		if got := strings.Join(parts, " "); strings.ReplaceAll(got, " ", "") != strings.ReplaceAll(line, " ", "") {
			t.Fatalf("limit %d: content changed: %q", limit, parts)
		}
		for _, p := range parts {
			if len(p) > limit || !utf8.ValidString(p) || strings.Count(p, "<a ") != strings.Count(p, "</a>") ||
				strings.Count(p, "&") != strings.Count(p, ";") {
				t.Fatalf("limit %d: bad part %q", limit, p)
			}
		}
	}
	// A single element longer than limit can only be cut inside it, but
	// never inside a rune.
	for _, p := range Split(strings.Repeat("é", 10), 5) {
		if !utf8.ValidString(p) || len(p) > 5 {
			t.Fatalf("bad part %q", p)
		}
	}
}

func TestIsRejected(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{&APIError{Code: 400, Description: "Bad Request: can't parse entities: unclosed tag"}, true},
		{&APIError{Code: 400, Description: "Bad Request: message is too long"}, true},
		{fmt.Errorf("wrapped: %w", &APIError{Code: 400, Description: "Bad Request: message text is empty"}), true},
		{&APIError{Code: 400, Description: "Bad Request: chat not found"}, false},
		{&APIError{Code: 429, Description: "Too Many Requests: retry after 5"}, false},
		{&APIError{Code: 403, Description: "Forbidden: bot was blocked by the user"}, false},
		{errors.New("can't parse entities"), false},
	}
	for _, tc := range cases {
		if got := IsRejected(tc.err); got != tc.want {
			t.Errorf("IsRejected(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

func TestSendFallsBackToPlainText(t *testing.T) {
	// plain: how the plain-text resend ends.
	for _, plain := range []string{"ok", "rejected", "down"} {
		var reqs []map[string]any
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var m map[string]any
			json.NewDecoder(r.Body).Decode(&m)
			reqs = append(reqs, m)
			switch {
			case m["parse_mode"] == "HTML" || plain == "rejected":
				w.WriteHeader(400)
				w.Write([]byte(`{"ok":false,"description":"Bad Request: can't parse entities"}`))
			case plain == "down":
				w.WriteHeader(502)
				w.Write([]byte(`{"ok":false,"description":"Bad Gateway"}`))
			default:
				w.Write([]byte(`{"ok":true}`))
			}
		}))
		c := &Client{HTTP: srv.Client(), BaseURL: srv.URL, Token: "T", ChatID: "1"}
		err := c.Send(context.Background(), `<b>A &amp; B</b> <a href="https://e.com/?a=1&amp;b=2">HN</a>`)
		srv.Close()
		if len(reqs) != 2 || reqs[1]["parse_mode"] != nil || reqs[1]["text"] != "A & B HN https://e.com/?a=1&b=2" {
			t.Fatalf("plain %s: bad requests %v", plain, reqs)
		}
		if (plain == "ok") != (err == nil) || (plain == "rejected") != IsRejected(err) {
			t.Fatalf("plain %s: err %v (rejected=%v)", plain, err, IsRejected(err))
		}
	}
}

func TestPlainTextClipsUTF16(t *testing.T) {
	s := PlainText(strings.Repeat("😀", MaxLen))
	n := 0
	for _, r := range s {
		n += utf16.RuneLen(r)
	}
	if n > MaxLen || !strings.HasSuffix(s, "…") {
		t.Fatalf("len %d", n)
	}
}

func TestSendHonorsRetryAfter(t *testing.T) {
	for _, tc := range []struct {
		retryAfter int
		wantOK     bool
		wantCalls  int
	}{
		{1, true, 2},     // waits 1s, then succeeds
		{3600, false, 1}, // too long to wait: fails at once
	} {
		calls := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if calls++; calls == 1 {
				w.WriteHeader(429)
				fmt.Fprintf(w, `{"ok":false,"description":"Too Many Requests","parameters":{"retry_after":%d}}`, tc.retryAfter)
				return
			}
			w.Write([]byte(`{"ok":true}`))
		}))
		c := &Client{HTTP: srv.Client(), BaseURL: srv.URL, Token: "T", ChatID: "1"}
		start := time.Now()
		err := c.Send(context.Background(), "hi")
		srv.Close()
		if (err == nil) != tc.wantOK || calls != tc.wantCalls {
			t.Fatalf("retry_after=%d: err=%v calls=%d", tc.retryAfter, err, calls)
		}
		if tc.wantOK && time.Since(start) < time.Second {
			t.Fatalf("retry_after=%d: did not wait", tc.retryAfter)
		}
	}
}

func TestSendRetryAfterStopsOnContext(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(429)
		w.Write([]byte(`{"ok":false,"description":"Too Many Requests","parameters":{"retry_after":30}}`))
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	c := &Client{HTTP: srv.Client(), BaseURL: srv.URL, Token: "T", ChatID: "1"}
	if err := c.Send(ctx, "hi"); err == nil || calls != 1 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}
