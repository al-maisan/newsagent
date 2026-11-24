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

var rEnv = map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "k", "TELEGRAM_BOT_TOKEN": "t",
	"REDDIT_CLIENT_ID": "i", "REDDIT_CLIENT_SECRET": "s"}

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
		"chat template":  `{"llm":{"interests":"x"},"telegram":{"chat_id":"REPLACE_WITH_CHAT_ID"}}`,
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

func TestLoadReddit(t *testing.T) {
	cases := map[string]string{
		`{"reddit":{"enabled":true}`:                                                  "subreddits is empty",
		`{"reddit":{"enabled":true,"subreddits":["r/golang"]}`:                        "not a valid subreddit",
		`{"reddit":{"enabled":true,"subreddits":["_go"]}`:                             "not a valid subreddit",
		`{"reddit":{"enabled":true,"subreddits":["golang","Golang"]}`:                 "listed twice",
		`{"reddit":{"enabled":true,"subreddits":["go"],"top_n":0}`:                    "top_n must be 1..100",
		`{"reddit":{"enabled":true,"subreddits":["go","a_b","c1234567890123456789"]}`: "",
	}
	for cfg, want := range cases {
		_, err := Load(write(t, cfg+`,"llm":{"interests":"x"},"telegram":{"chat_id":"1"}}`), env(rEnv))
		if want == "" && err != nil || want != "" && (err == nil || !strings.Contains(err.Error(), want)) {
			t.Errorf("%s: got %v, want %q", cfg, err, want)
		}
	}
	// Missing secrets.
	_, err := Load(write(t, `{"reddit":{"enabled":true,"subreddits":["go"]},"llm":{"interests":"x"},"telegram":{"chat_id":"1"}}`), env(baseEnv))
	if err == nil || !strings.Contains(err.Error(), "REDDIT_CLIENT_ID") {
		t.Errorf("missing reddit secrets: got %v", err)
	}
	// A feed may not shadow the reddit source name.
	if _, err := Load(write(t, `{"feeds":[{"name":"reddit","url":"https://a"}],"llm":{"interests":"x"},"telegram":{"chat_id":"1"}}`), env(baseEnv)); err == nil {
		t.Error("feed named reddit: want error")
	}
}

func TestLoadAcceptsChatIDsAndDisabledHN(t *testing.T) {
	for _, cfg := range []string{
		`{"llm":{"interests":"x"},"telegram":{"chat_id":"-1001234567890"}}`,
		`{"llm":{"interests":"x"},"telegram":{"chat_id":"@my_channel"}}`,
		`{"hn":{"enabled":false,"top_n":0},"llm":{"interests":"x"},"telegram":{"chat_id":"1"}}`,
	} {
		if _, err := Load(write(t, cfg), env(baseEnv)); err != nil {
			t.Errorf("%s: %v", cfg, err)
		}
	}
	if _, err := Load(write(t, `{"hn":{"enabled":true,"top_n":0},"llm":{"interests":"x"},"telegram":{"chat_id":"1"}}`), env(baseEnv)); err == nil {
		t.Error("enabled hn with top_n 0: want error")
	}
}

// The example config must fail only on its placeholders, so a verbatim
// copy can't run with a made-up chat or follow a random X account.
func TestExampleConfigPlaceholders(t *testing.T) {
	_, err := Load("../../deploy/config.example.json", env(xEnv))
	if err == nil {
		t.Fatal("example config with placeholders must not load")
	}
	lines := strings.Split(err.Error(), "\n")
	if len(lines) != 2 || !strings.Contains(err.Error(), "REPLACE_WITH_CHAT_ID") || !strings.Contains(err.Error(), "REPLACE_OR_REMOVE") {
		t.Fatalf("want exactly the two placeholder errors, got:\n%v", err)
	}
	b, rerr := os.ReadFile("../../deploy/config.example.json")
	if rerr != nil {
		t.Fatal(rerr)
	}
	fixed := strings.NewReplacer(`"REPLACE_WITH_CHAT_ID"`, `"12345"`, `"REPLACE_OR_REMOVE"`, `"real_account"`).Replace(string(b))
	c, err := Load(write(t, fixed), env(xEnv))
	if err != nil {
		t.Fatalf("example config with placeholders filled in must load: %v", err)
	}
	if h := c.PrivateHosts(); len(h) != 0 {
		t.Fatalf("public example feeds must all be checked, got exempt hosts %v", h)
	}
}

func TestPrivateHosts(t *testing.T) {
	c := &Config{Feeds: []Feed{
		{Name: "public", URL: "https://lobste.rs/rss"},
		{Name: "nas", URL: "http://nas.lan:8080/rss", Private: true},
		{Name: "ip", URL: "http://192.168.1.5/rss"},
		{Name: "ip6", URL: "http://[fd7a:115c::1]/rss"},
	}}
	got := strings.Join(c.PrivateHosts(), ",")
	if got != "nas.lan,192.168.1.5,fd7a:115c::1" {
		t.Fatalf("PrivateHosts = %q; a public feed host must never be exempt", got)
	}
}
