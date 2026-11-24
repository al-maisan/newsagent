package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, s string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "c.json")
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

var baseEnv = map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "k", "TELEGRAM_BOT_TOKEN": "t"}

var xEnv = map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "k", "TELEGRAM_BOT_TOKEN": "t",
	"X_CONSUMER_KEY": "a", "X_CONSUMER_SECRET": "b", "X_ACCESS_TOKEN": "c", "X_ACCESS_TOKEN_SECRET": "d"}

func TestLoadDefaults(t *testing.T) {
	p := write(t, `{"llm":{"interests":"tech"},"telegram":{"chat_id":"1"},"hn":{"enabled":true},
		"feeds":[{"name":"lwn","url":"https://lwn.net/headlines/rss"}]}`)
	c, err := Load(p, env(baseEnv))
	if err != nil {
		t.Fatal(err)
	}
	if c.LLM.Model != "claude-opus-5-5" || c.HN.TopN != 30 || time.Duration(c.MaxAge) != 48*time.Hour || c.Thresholds.Urgent != 9 {
		t.Fatalf("defaults not applied: %+v", c)
	}
}

func TestLoadErrors(t *testing.T) {
	cases := map[string]string{
		"unknown field":  `{"bogus":1,"llm":{"interests":"x"},"telegram":{"chat_id":"1"}}`,
		"x secrets":      `{"x":{"enabled":true},"llm":{"interests":"x"},"telegram":{"chat_id":"1"}}`,
		"feed scheme":    `{"feeds":[{"name":"a","url":"file:///etc/passwd"}],"llm":{"interests":"x"},"telegram":{"chat_id":"1"}}`,
		"duplicate feed": `{"feeds":[{"name":"hn","url":"https://a"}],"llm":{"interests":"x"},"telegram":{"chat_id":"1"}}`,
		"bad effort":     `{"llm":{"interests":"x","score_effort":"huge"},"telegram":{"chat_id":"1"}}`,
		"no chat":        `{"llm":{"interests":"x"}}`,
		"bad duration":   `{"max_age":"soon","llm":{"interests":"x"},"telegram":{"chat_id":"1"}}`,
		"max_age long":   `{"max_age":"720h","llm":{"interests":"x"},"telegram":{"chat_id":"1"}}`,
		"bad timezone":   `{"timezone":"Mars/Base","llm":{"interests":"x"},"telegram":{"chat_id":"1"}}`,
	}
	for name, cfg := range cases {
		if _, err := Load(write(t, cfg), env(baseEnv)); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
	_, err := Load(write(t, `{"llm":{"interests":"x"},"telegram":{"chat_id":"1"}}`), env(nil))
	if err == nil || !strings.Contains(err.Error(), "CLAUDE_CODE_OAUTH_TOKEN") {
		t.Errorf("missing secrets: got %v", err)
	}
}

func TestLoadX(t *testing.T) {
	cases := map[string]string{
		`{"x":{"enabled":true,"accounts":["@esr"]}`:              "not a valid username",
		`{"x":{"enabled":true,"home_timeline":false}`:            "all off",
		`{"x":{"enabled":true,"accounts":["hdmoore","HDMoore"]}`: "listed twice",
		`{"x":{"enabled":true,"accounts":["esr","a_b1"]}`:        "",
	}
	for prefix, want := range cases {
		_, err := Load(write(t, prefix+`,"llm":{"interests":"x"},"telegram":{"chat_id":"1"}}`), env(xEnv))
		if want == "" && err != nil || want != "" && (err == nil || !strings.Contains(err.Error(), want)) {
			t.Errorf("%s: got %v, want %q", prefix, err, want)
		}
	}
}
