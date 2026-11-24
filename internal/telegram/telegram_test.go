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

func TestIsRejected(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{&APIError{400, "Bad Request: can't parse entities: unclosed tag"}, true},
		{&APIError{400, "Bad Request: message is too long"}, true},
		{fmt.Errorf("wrapped: %w", &APIError{400, "Bad Request: message text is empty"}), true},
		{&APIError{400, "Bad Request: chat not found"}, false},
		{&APIError{429, "Too Many Requests: retry after 5"}, false},
		{&APIError{403, "Forbidden: bot was blocked by the user"}, false},
		{errors.New("can't parse entities"), false},
	}
	for _, tc := range cases {
		if got := IsRejected(tc.err); got != tc.want {
			t.Errorf("IsRejected(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}
