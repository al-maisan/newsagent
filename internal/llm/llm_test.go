package llm

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/al-maisan/newsagent/internal/store"
)

// fakeCLI installs a fake claude binary that records its args, stdin and
// environment, prints result and exits with code.
func fakeCLI(t *testing.T, result string, code int) (*Claude, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "out"), []byte(result), 0o600); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nd=" + dir + "\ncat > $d/stdin\nprintf '%s\\n' \"$@\" > $d/args\nenv > $d/env\ncat $d/out\nexit " +
		string(rune('0'+code)) + "\n"
	bin := filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ANTHROPIC_API_KEY", "must-not-leak")
	return &Claude{Path: bin, HomeDir: dir, OAuthToken: "tok", Model: "claude-opus-5-5",
		ScoreEffort: "low", DigestEffort: "medium", Interests: "tech"}, dir
}

func success(structured string) string {
	return `{"type":"result","subtype":"success","is_error":false,"result":"","structured_output":` + structured + `}`
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestScoreValidates(t *testing.T) {
	c, dir := fakeCLI(t, success(`{"items":[
		{"id":1,"score":15,"summary":"a","duplicate":false},
		{"id":1,"score":3,"summary":"repeat","duplicate":false},
		{"id":99,"score":10,"summary":"invented","duplicate":false},
		{"id":2,"score":-4,"summary":"b","duplicate":true}]}`), 0)
	scores, err := c.Score(context.Background(), []store.Item{{ID: 1, Title: "x"}, {ID: 2, Title: "y"}}, []string{"old story"})
	if err != nil {
		t.Fatal(err)
	}
	if len(scores) != 2 || scores[0].Score != 10 || scores[1].Score != 0 || !scores[1].Duplicate {
		t.Fatalf("bad scores %+v", scores)
	}

	args := read(t, filepath.Join(dir, "args"))
	for _, want := range []string{"-p\n", "--model\nclaude-opus-5-5\n", "--effort\nlow\n", "--tools\n\n", "--output-format\njson\n", "--json-schema\n"} {
		if !strings.Contains(args, want) {
			t.Errorf("args lack %q:\n%s", want, args)
		}
	}
	var in struct {
		AlreadyReported []string          `json:"already_reported"`
		Items           []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal([]byte(read(t, filepath.Join(dir, "stdin"))), &in); err != nil || len(in.Items) != 2 || in.AlreadyReported[0] != "old story" {
		t.Fatalf("bad stdin payload: %v %+v", err, in)
	}
	env := read(t, filepath.Join(dir, "env"))
	if strings.Contains(env, "must-not-leak") || !strings.Contains(env, "CLAUDE_CODE_OAUTH_TOKEN=tok") {
		t.Fatalf("bad env:\n%s", env)
	}
}

func TestDigest(t *testing.T) {
	c, _ := fakeCLI(t, success(`{"sections":[{"heading":"AI","entries":[{"text":"t","item_ids":[1]}]}]}`), 0)
	s, err := c.Digest(context.Background(), []store.Item{{ID: 1}})
	if err != nil || len(s) != 1 || s[0].Entries[0].ItemIDs[0] != 1 {
		t.Fatalf("got %+v %v", s, err)
	}
}

func TestCallErrors(t *testing.T) {
	cases := []struct {
		name      string
		out       string
		code      int
		transient bool
	}{
		{"crash without json", "boom", 1, true},
		{"usage limit", `{"subtype":"success","is_error":true,"result":"Claude usage limit reached. Resets 5pm"}`, 1, true},
		{"overloaded", `{"subtype":"success","is_error":true,"result":"overloaded","api_error_status":529}`, 1, true},
		{"bad request", `{"subtype":"success","is_error":true,"result":"invalid model","api_error_status":400}`, 1, false},
		{"max turns", `{"subtype":"error_max_turns","is_error":true,"result":""}`, 1, false},
		{"no structured output", `{"subtype":"success","is_error":false,"result":"hi"}`, 0, false},
	}
	for _, tc := range cases {
		c, _ := fakeCLI(t, tc.out, tc.code)
		_, err := c.Digest(context.Background(), []store.Item{{ID: 1}})
		if err == nil {
			t.Errorf("%s: want error", tc.name)
			continue
		}
		if IsTransient(err) != tc.transient {
			t.Errorf("%s: transient=%v, want %v (%v)", tc.name, IsTransient(err), tc.transient, err)
		}
	}
	if IsTransient(errors.New("x")) {
		t.Error("plain error must not be transient")
	}
}

func TestCallTimeout(t *testing.T) {
	waitDelay = 100 * time.Millisecond
	t.Cleanup(func() { waitDelay = 5 * time.Second })
	dir := t.TempDir()
	// The child sleep keeps stdout open after the shell is killed.
	bin := filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\ncat >/dev/null\nsleep 30\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	c := &Claude{Path: bin, HomeDir: dir, Model: "m", DigestEffort: "low", Timeout: 200 * time.Millisecond}
	start := time.Now()
	_, err := c.Digest(context.Background(), []store.Item{{ID: 1}})
	if err == nil || !IsTransient(err) || !strings.Contains(err.Error(), "no answer within") {
		t.Fatalf("want transient timeout error, got %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("timeout took %s", d)
	}
}

func TestUsageLimit(t *testing.T) {
	c, _ := fakeCLI(t, `{"type":"result","subtype":"success","is_error":true,"result":"You've hit your 5-hour limit · resets 7:30am (UTC)"}`, 1)
	_, err := c.Digest(context.Background(), []store.Item{{ID: 1}})
	if !IsUsageLimit(err) || !IsTransient(err) || !strings.Contains(err.Error(), "resets 7:30am (UTC)") {
		t.Fatalf("want usage-limit error with the CLI message, got %v", err)
	}
	c, _ = fakeCLI(t, `{"subtype":"success","is_error":true,"result":"API Error: 429 {\"type\":\"error\",\"error\":{\"type\":\"rate_limit_error\",\"message\":\"Number of request tokens has exceeded your per-minute rate limit\"}}","api_error_status":429}`, 1)
	if _, err := c.Digest(context.Background(), []store.Item{{ID: 1}}); IsUsageLimit(err) || !IsTransient(err) {
		t.Fatalf("a per-minute rate limit is transient but not the plan's usage limit: %v", err)
	}
	c, _ = fakeCLI(t, `{"subtype":"success","is_error":true,"result":"overloaded","api_error_status":529}`, 1)
	if _, err := c.Digest(context.Background(), []store.Item{{ID: 1}}); IsUsageLimit(err) || !IsTransient(err) {
		t.Fatalf("overload is transient but not a usage limit: %v", err)
	}
}
