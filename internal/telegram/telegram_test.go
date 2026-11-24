package telegram

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
