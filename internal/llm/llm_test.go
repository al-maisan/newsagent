package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/al-maisan/newsagent/internal/store"
)

// fakeAPI answers every request with a message whose text is out.
func fakeAPI(t *testing.T, stop, out string, got *map[string]any) *Claude {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got != nil {
			json.NewDecoder(r.Body).Decode(got)
		}
		w.Header().Set("Content-Type", "application/json")
		text, _ := json.Marshal(out)
		fmt.Fprintf(w, `{"id":"m","type":"message","role":"assistant","model":"claude-opus-5-5",
			"content":[{"type":"text","text":%s}],"stop_reason":%q,"usage":{"input_tokens":1,"output_tokens":1}}`, text, stop)
	}))
	t.Cleanup(srv.Close)
	return &Claude{
		Client:      anthropic.NewClient(option.WithAPIKey("k"), option.WithBaseURL(srv.URL), option.WithMaxRetries(0)),
		Model:       "claude-opus-5-5",
		ScoreEffort: "low", DigestEffort: "medium", Interests: "tech",
	}
}

func TestScoreValidates(t *testing.T) {
	var req map[string]any
	c := fakeAPI(t, "end_turn", `{"items":[
		{"id":1,"score":15,"summary":"a","duplicate":false},
		{"id":1,"score":3,"summary":"repeat","duplicate":false},
		{"id":99,"score":10,"summary":"invented","duplicate":false},
		{"id":2,"score":-4,"summary":"b","duplicate":true}]}`, &req)
	scores, err := c.Score(context.Background(), []store.Item{{ID: 1, Title: "x"}, {ID: 2, Title: "y"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(scores) != 2 || scores[0].Score != 10 || scores[1].Score != 0 || !scores[1].Duplicate {
		t.Fatalf("bad scores %+v", scores)
	}
	oc, _ := req["output_config"].(map[string]any)
	if req["model"] != "claude-opus-5-5" || oc["effort"] != "low" || req["fallbacks"] != "default" {
		t.Fatalf("bad request %v", req)
	}
}

func TestScoreRejectsBadStop(t *testing.T) {
	c := fakeAPI(t, "max_tokens", `{"items":[`, nil)
	if _, err := c.Score(context.Background(), []store.Item{{ID: 1}}, nil); err == nil {
		t.Fatal("want error on truncated output")
	}
}

func TestDigest(t *testing.T) {
	c := fakeAPI(t, "end_turn", `{"sections":[{"heading":"AI","entries":[{"text":"t","item_ids":[1]}]}]}`, nil)
	s, err := c.Digest(context.Background(), []store.Item{{ID: 1}})
	if err != nil || len(s) != 1 || s[0].Entries[0].ItemIDs[0] != 1 {
		t.Fatalf("got %+v %v", s, err)
	}
}

func TestIsTransient(t *testing.T) {
	if !IsTransient(&anthropic.Error{StatusCode: 529}) || !IsTransient(&anthropic.Error{StatusCode: 429}) {
		t.Fatal("overload/rate limit must be transient")
	}
	if IsTransient(&anthropic.Error{StatusCode: 400}) || IsTransient(errors.New("decode")) {
		t.Fatal("bad request must not be transient")
	}
	if !IsTransient(context.DeadlineExceeded) {
		t.Fatal("timeout must be transient")
	}
}
