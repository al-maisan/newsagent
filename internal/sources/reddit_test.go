package sources

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/al-maisan/newsagent/internal/store"
)

// redditSrv serves the token endpoint and one hot listing per subreddit.
func redditSrv(t *testing.T, tokenStatus int, hot map[string]string) (*httptest.Server, *[]string) {
	t.Helper()
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/access_token":
			calls = append(calls, "token")
			if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" ||
				r.ParseForm() != nil || r.PostForm.Get("grant_type") != "client_credentials" {
				t.Errorf("bad token request: %v %v", r.Method, r.PostForm)
			}
			if got := base64.StdEncoding.EncodeToString([]byte("id:secret")); r.Header.Get("Authorization") != "Basic "+got {
				t.Errorf("token request must carry basic auth, got %q", r.Header.Get("Authorization"))
			}
			if tokenStatus != 0 {
				w.WriteHeader(tokenStatus)
				fmt.Fprint(w, `{"error":"invalid_grant"}`)
				return
			}
			fmt.Fprint(w, `{"access_token":"TOK","expires_in":3600}`)
		case strings.HasPrefix(r.URL.Path, "/r/") && strings.HasSuffix(r.URL.Path, "/hot"):
			calls = append(calls, r.URL.Path+"?"+r.URL.RawQuery)
			if r.Header.Get("Authorization") != "Bearer TOK" {
				t.Errorf("listing must carry the bearer token, got %q", r.Header.Get("Authorization"))
			}
			if strings.Contains(r.URL.RawQuery, "raw_json=1") && !strings.Contains(r.URL.RawQuery, "limit=30") {
				t.Errorf("bad query %q", r.URL.RawQuery)
			}
			sub := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/r/"), "/hot")
			if body, ok := hot[sub]; ok {
				fmt.Fprint(w, body)
				return
			}
			http.Error(w, "no such sub", http.StatusNotFound)
		default:
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

const redditLink = `{"kind":"Listing","data":{"children":[
	{"kind":"t3","data":{"id":"a1","title":"Go 2.0 announced","url":"https://go.dev/blog/go2",
		"permalink":"/r/golang/comments/a1/go_20_announced/","author":"rob","created_utc":1764582000.5,
		"score":84,"num_comments":12}},
	{"kind":"t3","data":{"id":"a2","title":"Pinned rules","url":"https://www.reddit.com/r/golang/comments/a2/pinned/",
		"permalink":"/r/golang/comments/a2/pinned/","author":"mod","created_utc":1764581000,"score":1,"stickied":true}},
	{"kind":"t3","data":{"id":"a3","title":"Ask HN style question","url":"https://www.reddit.com/r/golang/comments/a3/ask/",
		"permalink":"/r/golang/comments/a3/ask/","author":"new","created_utc":1764583000,"score":2,
		"selftext":"How do I <b>convert</b> a string to an int?"}},
	{"kind":"t3","data":{"id":"a4","title":"NSFW post","url":"https://example.com/x","permalink":"/r/golang/comments/a4/x/",
		"author":"x","created_utc":1764584000,"score":5,"over_18":true}}
]}}`

func newReddit(srv *httptest.Server) *Reddit {
	return &Reddit{Client: srv.Client(), BaseURL: srv.URL, TokenURL: srv.URL + "/api/v1/access_token",
		ClientID: "id", ClientSecret: "secret", Subreddits: []string{"golang"}, TopN: 30}
}

func TestRedditFetch(t *testing.T) {
	srv, calls := redditSrv(t, 0, map[string]string{
		"golang": redditLink,
		"rust":   `{"kind":"Listing","data":{"children":[{"kind":"t3","data":{"id":"b1","title":"Rust 2.0","url":"https://rust.org/2","permalink":"/r/rust/comments/b1/rust_20/","author":"steve","created_utc":1764585000,"score":9,"num_comments":3}}]}}`,
	})
	r := newReddit(srv)
	r.Subreddits = []string{"golang", "rust"}
	items, err := r.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 {
		t.Fatalf("want 3 items (stickied and over_18 dropped), got %+v", items)
	}
	got := items[0]
	want := store.Item{
		Source:     "reddit",
		ExtID:      "a1",
		Title:      "Go 2.0 announced",
		URL:        "https://go.dev/blog/go2",
		DiscussURL: "https://www.reddit.com/r/golang/comments/a1/go_20_announced/",
		Author:     "rob",
		Text:       "84 points, 12 comments",
		Published:  time.Unix(1764582000, 0).UTC(),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("bad item %+v, want %+v", got, want)
	}
	// Self post: URL falls back to the permalink; the selftext is cleaned
	// and added to the model context.
	if items[1].URL != items[1].DiscussURL || items[1].Text != "2 points, 0 comments · How do I convert a string to an int?" {
		t.Fatalf("bad self post %+v", items[1])
	}
	if items[2].ExtID != "b1" {
		t.Fatalf("bad last item %+v", items[2])
	}
	if len(*calls) != 3 { // one token, two subreddits
		t.Fatalf("want 3 requests, got %v", *calls)
	}
	// Second run: no cross-run state, same requests.
	if _, err := r.Fetch(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRedditOneSubFails(t *testing.T) {
	srv, _ := redditSrv(t, 0, map[string]string{"golang": redditLink})
	r := newReddit(srv)
	r.Subreddits = []string{"gone", "golang"}
	items, err := r.Fetch(context.Background())
	if err != nil {
		t.Fatalf("one failing subreddit must not fail the fetch: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("want the working subreddit's items, got %+v", items)
	}
}

func TestRedditAllSubsFail(t *testing.T) {
	srv, _ := redditSrv(t, 0, nil)
	r := newReddit(srv)
	if _, err := r.Fetch(context.Background()); err == nil || !strings.Contains(err.Error(), "r/golang") {
		t.Fatalf("want an error naming the subreddit, got %v", err)
	}
}

func TestRedditAuthAlarm(t *testing.T) {
	rec := &recAlarms{}
	cases := []struct {
		tokenStatus int
		hot         map[string]int // subreddit -> status
		raise       bool
		clear       bool
	}{
		{401, nil, true, false}, // bad id/secret: raise
		{0, map[string]int{"golang": 401}, true, false},
		{0, map[string]int{"golang": 0}, false, true}, // works again: clear
	}
	for _, tc := range cases {
		var srv *httptest.Server
		if len(tc.hot) == 1 && tc.hot["golang"] != 0 {
			srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/access_token" {
					fmt.Fprint(w, `{"access_token":"TOK"}`)
					return
				}
				http.Error(w, `{"message":"Unauthorized"}`, tc.hot["golang"])
			}))
		} else {
			srv, _ = redditSrv(t, tc.tokenStatus, map[string]string{"golang": redditLink})
		}
		rec.raised, rec.cleared = nil, nil
		r := newReddit(srv)
		r.Alarms = rec
		r.Fetch(context.Background())
		srv.Close()
		if (len(rec.raised) > 0) != tc.raise || (len(rec.cleared) > 0) != tc.clear {
			t.Fatalf("token=%d hot=%v: raised=%v cleared=%v", tc.tokenStatus, tc.hot, rec.raised, rec.cleared)
		}
	}
	// A 403 is not an auth problem (blocked user agent, private subreddit).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/access_token" {
			fmt.Fprint(w, `{"access_token":"TOK"}`)
			return
		}
		http.Error(w, `{"message":"Forbidden"}`, http.StatusForbidden)
	}))
	defer srv.Close()
	rec.raised, rec.cleared = nil, nil
	r := newReddit(srv)
	r.Alarms = rec
	r.Fetch(context.Background())
	if len(rec.raised) != 0 {
		t.Fatalf("403 must not raise the auth alarm: %v", rec.raised)
	}
}
