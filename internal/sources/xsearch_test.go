package sources

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/al-maisan/newsagent/internal/store"
)

// xsServer answers advanced_search with the pages in pages (by cursor) and
// records the queries it got.
func xsServer(t *testing.T, pages map[string]string, status *int) (*httptest.Server, *[]string) {
	t.Helper()
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "k" || r.URL.Path != "/twitter/tweet/advanced_search" || r.URL.Query().Get("queryType") != "Latest" {
			http.Error(w, `{"error":"bad request"}`, 400)
			return
		}
		queries = append(queries, r.URL.Query().Get("query"))
		if status != nil && *status != 0 {
			http.Error(w, `{"error":"x"}`, *status)
			return
		}
		body, ok := pages[r.URL.Query().Get("cursor")]
		if !ok {
			body = `{"tweets":[],"has_next_page":false,"next_cursor":""}`
		}
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, &queries
}

func xsTweet(id, user string) string {
	return `{"id":"` + id + `","url":"https://x.com/` + user + `/status/` + id + `","text":"hi &amp; bye","createdAt":"Tue Nov 24 10:59:02 +0000 2026","likeCount":3,"retweetCount":1,"replyCount":2,"author":{"userName":"` + user + `"}}`
}

func TestXSearchFetch(t *testing.T) {
	pages := map[string]string{
		// Not sorted by id: the newest id is in the middle.
		"":   `{"tweets":[` + xsTweet("1005", "alice") + `,` + xsTweet("1009", "bob") + `],"has_next_page":true,"next_cursor":"c2"}`,
		"c2": `{"tweets":[` + xsTweet("1001", "alice") + `],"has_next_page":false,"next_cursor":""}`,
	}
	srv, queries := xsServer(t, pages, nil)
	x := &XSearch{Client: srv.Client(), BaseURL: srv.URL, Key: "k", Accounts: []string{"Alice", "bob"},
		ExcludeReplies: true, MinInterval: 15 * time.Minute, MaxPerDay: 100}
	kv := memKV{}
	now := time.Date(2026, 11, 24, 11, 0, 0, 0, time.UTC)
	items, err := x.Fetch(context.Background(), kv, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 || items[1].ExtID != "1009" || items[1].Source != "x" || items[1].Title != "@bob: hi & bye" ||
		items[1].URL != "https://x.com/bob/status/1009" || !items[1].Published.Equal(time.Date(2026, 11, 24, 10, 59, 2, 0, time.UTC)) ||
		!strings.Contains(items[1].Text, "likes 3, reposts 1, replies 2") {
		t.Fatalf("bad items %+v", items)
	}
	first := (*queries)[0]
	if len(*queries) != 2 || !strings.HasPrefix(first, "(from:alice OR from:bob) -filter:replies since_time:") {
		t.Fatalf("bad first queries %q", *queries)
	}
	q := x.queries()[0]
	if kv[kvXSSince+q.key] != "1009" || kv[kvXSReads+"2026-11-24"] != "3" {
		t.Fatalf("state not saved: %v", kv)
	}

	// Too early: no request.
	if items, _ := x.Fetch(context.Background(), kv, now.Add(5*time.Minute)); items != nil || len(*queries) != 2 {
		t.Fatalf("polled before min_interval: %q", *queries)
	}
	// Next poll: only posts newer than the newest seen.
	for k := range pages {
		delete(pages, k)
	}
	if items, err := x.Fetch(context.Background(), kv, now.Add(20*time.Minute)); err != nil || len(items) != 0 {
		t.Fatalf("second poll: %v %v", items, err)
	}
	if got := (*queries)[2]; got != "(from:alice OR from:bob) -filter:replies since_id:1009" {
		t.Fatalf("bad follow-up query %q", got)
	}
}

func TestXSearchQueriesFit(t *testing.T) {
	var accounts []string
	for i := range 60 {
		accounts = append(accounts, fmt.Sprintf("account_%07d", i)) // 15 chars, the maximum
	}
	x := &XSearch{Accounts: accounts, ExcludeReplies: true}
	qs := x.queries()
	if len(qs) < 2 {
		t.Fatalf("want several queries, got %d", len(qs))
	}
	var seen []string
	for _, q := range qs {
		if n := len(q.text + " since_id:99999999999999999999"); n > xsMaxQuery {
			t.Fatalf("query too long: %d", n)
		}
		for _, part := range strings.Split(strings.TrimSuffix(strings.TrimPrefix(q.text, "(from:"), ") -filter:replies"), " OR from:") {
			seen = append(seen, part)
		}
	}
	slices.Sort(seen)
	if !slices.Equal(seen, accounts) {
		t.Fatalf("accounts lost or repeated: %d of %d", len(seen), len(accounts))
	}
	if qs[0].key == qs[1].key || x.queries()[0].key != qs[0].key {
		t.Fatal("query keys must be distinct and stable")
	}
}

func TestXSearchErrorKeepsSinceID(t *testing.T) {
	pages := map[string]string{"": `{"tweets":[` + xsTweet("2002", "alice") + `],"has_next_page":true,"next_cursor":"c2"}`}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("cursor") == "c2" {
			http.Error(w, `{"detail":"retry"}`, 503)
			return
		}
		fmt.Fprint(w, pages[""])
	}))
	defer srv.Close()
	x := &XSearch{Client: srv.Client(), BaseURL: srv.URL, Key: "k", Accounts: []string{"alice"}, MinInterval: time.Minute, MaxPerDay: 100}
	kv := memKV{}
	items, err := x.Fetch(context.Background(), kv, time.Now())
	if err != nil || len(items) != 1 {
		t.Fatalf("page 1 must be kept: %v %v", items, err)
	}
	if v := kv[kvXSSince+x.queries()[0].key]; v != "" {
		t.Fatalf("since_id advanced after a failed page: %q", v)
	}
}

func TestXSearchDailyCap(t *testing.T) {
	srv, queries := xsServer(t, map[string]string{}, nil)
	x := &XSearch{Client: srv.Client(), BaseURL: srv.URL, Key: "k", Accounts: []string{"alice"}, MinInterval: time.Minute, MaxPerDay: 50}
	now := time.Date(2026, 11, 24, 11, 0, 0, 0, time.UTC)
	kv := memKV{kvXSReads + "2026-11-24": "50", kvXSReads + "2026-11-23": "9"}
	if items, err := x.Fetch(context.Background(), kv, now); items != nil || err != nil || len(*queries) != 0 {
		t.Fatalf("read over the daily cap: %v %v %q", items, err, *queries)
	}
	if _, ok := kv[kvXSReads+"2026-11-23"]; ok {
		t.Error("old counter not dropped")
	}
	kv[kvXSReads+"2026-11-24"] = "junk"
	if _, err := x.Fetch(context.Background(), kv, now); err == nil {
		t.Fatal("corrupt counter must fail closed")
	}
}

func TestXSearchAlarms(t *testing.T) {
	status := 402
	srv, _ := xsServer(t, map[string]string{}, &status)
	rec := &recAlarms{}
	x := &XSearch{Client: srv.Client(), BaseURL: srv.URL, Key: "k", Accounts: []string{"alice"}, MinInterval: time.Minute, MaxPerDay: 100, Alarms: rec}
	kv := memKV{}
	now := time.Date(2026, 11, 24, 11, 0, 0, 0, time.UTC)
	poll := func() error {
		now = now.Add(time.Hour)
		rec.raised, rec.cleared = nil, nil
		_, err := x.Fetch(context.Background(), kv, now)
		return err
	}
	if err := poll(); err == nil || !slices.Contains(rec.raised, AlarmXSearchCredits) || slices.Contains(rec.cleared, AlarmXSearchCredits) {
		t.Fatalf("402: err=%v raised=%v cleared=%v", err, rec.raised, rec.cleared)
	}
	status = 401
	if poll(); !slices.Contains(rec.raised, AlarmXSearchAuth) {
		t.Fatalf("401 must raise the auth alarm: %v", rec.raised)
	}
	status = 0
	if err := poll(); err != nil || !slices.Contains(rec.cleared, AlarmXSearchCredits) || !slices.Contains(rec.cleared, AlarmXSearchAuth) {
		t.Fatalf("success must clear both: err=%v cleared=%v", err, rec.cleared)
	}
}

func TestXSearchAlwaysDigest(t *testing.T) {
	pages := map[string]string{"": `{"tweets":[` + xsTweet("3001", "Bell") + `,` + xsTweet("3002", "other") + `],"has_next_page":false,"next_cursor":""}`}
	srv, _ := xsServer(t, pages, nil)
	x := &XSearch{Client: srv.Client(), BaseURL: srv.URL, Key: "k", Accounts: []string{"bell", "other"}, AlwaysDigest: []string{"bell"},
		MinInterval: time.Minute, MaxPerDay: 100}
	items, err := x.Fetch(context.Background(), memKV{}, time.Now())
	if err != nil || len(items) != 2 {
		t.Fatal(items, err)
	}
	if items[0].Priority != store.PriorityDigest || items[1].Priority != "" {
		t.Fatalf("priorities: %q %q", items[0].Priority, items[1].Priority)
	}
}

func TestXSearchReposts(t *testing.T) {
	repost := `{"id":"5009","text":"RT @orig: short…","createdAt":"Tue Nov 24 06:21:32 +0000 2026","likeCount":617,"isRetweet":true,
		"author":{"userName":"Bell"},
		"retweeted_tweet":{"id":"4001","text":"the full original text","createdAt":"Mon Nov 20 02:12:15 +0000 2026","likeCount":617,"retweetCount":9,"replyCount":4,"author":{"userName":"orig"}}}`
	broken := `{"id":"5005","text":"RT @gone: x","createdAt":"Tue Nov 24 05:00:00 +0000 2026","isRetweet":true,"author":{"userName":"Bell"}}`
	pages := map[string]string{"": `{"tweets":[` + repost + `,` + broken + `],"has_next_page":false,"next_cursor":""}`}
	srv, queries := xsServer(t, pages, nil)
	x := &XSearch{Client: srv.Client(), BaseURL: srv.URL, Key: "k", Accounts: []string{"bell"}, AlwaysDigest: []string{"bell"},
		IncludeReposts: true, ExcludeReplies: true, MinInterval: time.Minute, MaxPerDay: 100}
	kv := memKV{}
	items, err := x.Fetch(context.Background(), kv, time.Now())
	if err != nil || len(items) != 2 {
		t.Fatal(items, err)
	}
	if q := (*queries)[0]; !strings.HasPrefix(q, "(from:bell) -filter:replies include:nativeretweets since_time:") {
		t.Fatalf("bad query %q", q)
	}
	it := items[0]
	if it.ExtID != "4001" || it.URL != "https://x.com/orig/status/4001" || it.Title != "@Bell reposted @orig: the full original text" ||
		it.Author != "Bell" || it.Priority != store.PriorityDigest || !strings.Contains(it.Text, "reposts 9, replies 4") ||
		!it.Published.Equal(time.Date(2026, 11, 24, 6, 21, 32, 0, time.UTC)) {
		t.Fatalf("bad repost item %+v", it)
	}
	// No original attached: kept as the plain repost.
	if items[1].ExtID != "5005" || items[1].Title != "@Bell: RT @gone: x" {
		t.Fatalf("bad fallback %+v", items[1])
	}
	// since_id follows the repost ids, not the original's.
	if kv[kvXSSince+x.queries()[0].key] != "5009" {
		t.Fatalf("since_id %v", kv)
	}
}
