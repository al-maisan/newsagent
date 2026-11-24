package sources

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/al-maisan/newsagent/internal/store"
)

// Test vector from X's "Creating a signature" documentation.
func TestOAuth1Signature(t *testing.T) {
	o := OAuth1{
		ConsumerKey:    "xvz1evFS4wEEPTGEFPHBog",
		ConsumerSecret: "kAcSOqF21Fu85e7zjz7ZN2U4ZRhfV3WpwPAoE3Z7kBw",
		Token:          "370773112-GmHxMAgYyLbNEtIKZeRNFsMKPR9EyMZeS9weJAEb",
		TokenSecret:    "LswwdoUaIvS8ltyTt5jkRh4J50vUPVVHtR2YPi5kE",
	}
	params := url.Values{
		"status":           {"Hello Ladies + Gentlemen, a signed OAuth request!"},
		"include_entities": {"true"},
	}
	h := o.headerWith("POST", "https://api.twitter.com/1.1/statuses/update.json", params,
		"kYjzVBB8Y0ZFabxSWbWovY3uYSQ2pTgmZeNu2VS4cg", "1318622958")
	want := `oauth_signature="` + pctEncode("hCtSmYh+iHYCEqBWrE7C7hYmtUk=") + `"`
	if !strings.Contains(h, want) {
		t.Fatalf("header %q lacks %q", h, want)
	}
}

func TestPctEncode(t *testing.T) {
	if got := pctEncode("a b+c~/é"); got != "a%20b%2Bc~%2F%C3%A9" {
		t.Fatalf("got %q", got)
	}
}

func TestParseFeedRSS(t *testing.T) {
	body := `<?xml version="1.0"?><rss version="2.0" xmlns:dc="http://purl.org/dc/elements/1.1/"><channel>
<item><title>Big &amp; news</title><link>https://example.com/a</link><guid>id-a</guid>
<description>&lt;p&gt;Some &lt;b&gt;text&lt;/b&gt;&lt;/p&gt;</description><comments>https://lobste.rs/s/a</comments>
<dc:creator>alice</dc:creator><pubDate>Mon, 23 Nov 2026 10:00:00 +0000</pubDate></item>
<item><title>Bad link</title><link>javascript:alert(1)</link></item>
<item><title></title><link>https://example.com/empty-title</link></item>
</channel></rss>`
	items, err := ParseFeed("lobsters", []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("want 1 item, got %d: %+v", len(items), items)
	}
	it := items[0]
	if it.Title != "Big & news" || it.ExtID != "id-a" || it.Text != "Some text" || it.Author != "alice" ||
		it.DiscussURL != "https://lobste.rs/s/a" || it.Published != time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC) {
		t.Fatalf("unexpected item %+v", it)
	}
}

func TestParseFeedAtom(t *testing.T) {
	body := `<feed xmlns="http://www.w3.org/2005/Atom"><entry><title>Optech #400</title><id>tag:x,1</id>
<link rel="self" href="https://example.com/self"/><link href="https://bitcoinops.org/n/400"/>
<summary>Weekly news</summary><updated>2026-11-20T00:00:00Z</updated></entry></feed>`
	items, err := ParseFeed("optech", []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].URL != "https://bitcoinops.org/n/400" || items[0].ExtID != "tag:x,1" {
		t.Fatalf("unexpected %+v", items)
	}
}

func TestParseFeedUnknown(t *testing.T) {
	if _, err := ParseFeed("x", []byte(`<html></html>`)); err == nil {
		t.Fatal("want error")
	}
}

func TestHNFetch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/topstories.json":
			fmt.Fprint(w, `[1,2,3,4]`)
		case "/item/1.json":
			fmt.Fprint(w, `{"id":1,"type":"story","title":"Story","url":"https://e.com","score":100,"descendants":5,"by":"u","time":1700000000}`)
		case "/item/2.json":
			fmt.Fprint(w, `{"id":2,"type":"story","title":"Ask HN: x","score":10,"time":1700000000}`)
		case "/item/3.json":
			fmt.Fprint(w, `{"id":3,"type":"job","title":"Job"}`)
		default:
			http.Error(w, "nope", 500)
		}
	}))
	defer srv.Close()
	h := &HN{Client: srv.Client(), BaseURL: srv.URL, TopN: 10}
	items, err := h.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("want 2 stories, got %+v", items)
	}
	if items[0].URL != "https://e.com" || items[0].Text != "100 points, 5 comments" {
		t.Fatalf("bad item %+v", items[0])
	}
	if items[1].URL != "https://news.ycombinator.com/item?id=2" {
		t.Fatalf("ask HN should link discussion, got %q", items[1].URL)
	}
}

type memKV map[string]string

func (m memKV) Get(_ context.Context, k string) (string, error) { return m[k], nil }
func (m memKV) Set(_ context.Context, k, v string) error        { m[k] = v; return nil }
func (m memKV) DeleteBefore(_ context.Context, prefix, bound string) error {
	for k := range m {
		if strings.HasPrefix(k, prefix) && k < bound {
			delete(m, k)
		}
	}
	return nil
}

func TestXFetch(t *testing.T) {
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "OAuth ") {
			http.Error(w, "no auth", 401)
			return
		}
		calls = append(calls, r.URL.Path+"?"+r.URL.RawQuery)
		switch r.URL.Path {
		case "/users/me":
			fmt.Fprint(w, `{"data":{"id":"42"}}`)
		case "/users/42/timelines/reverse_chronological":
			fmt.Fprint(w, `{"data":[{"id":"900","text":"hello &amp; world","author_id":"7","created_at":"2026-11-23T09:00:00.000Z","public_metrics":{"like_count":3}}],
				"meta":{"newest_id":"900","result_count":1}}`)
		case "/users":
			fmt.Fprint(w, `{"data":[{"id":"7","username":"bob"}]}`)
		}
	}))
	defer srv.Close()
	x := &X{Client: srv.Client(), BaseURL: srv.URL, Auth: OAuth1{"a", "b", "c", "d"},
		MaxPerPoll: 50, MaxPerDay: 60, MinInterval: 30 * time.Minute, ExcludeReplies: true, HomeTimeline: true}
	kv := memKV{kvXReads + "2020-01-01": "7"} // an old day's counter
	now := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)

	items, err := x.Fetch(context.Background(), kv, now)
	if _, ok := kv[kvXReads+"2020-01-01"]; ok {
		t.Error("old read counter not dropped")
	}
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].URL != "https://x.com/bob/status/900" || items[0].Title != "@bob: hello & world" {
		t.Fatalf("unexpected %+v", items)
	}
	if kv[kvXSinceID] != "900" || kv[kvXUserID] != "42" || kv[kvXReads+"2026-11-23"] != "1" {
		t.Fatalf("state not saved: %v", kv)
	}
	if !strings.Contains(calls[1], "max_results=50") || !strings.Contains(calls[1], "exclude=replies") || strings.Contains(calls[1], "expansions") {
		t.Fatalf("bad query %q", calls[1])
	}
	// The author's name was looked up once and cached.
	if !strings.HasPrefix(calls[2], "/users?ids=7&") || !strings.HasPrefix(kv[kvXName+"7"], "bob ") {
		t.Fatalf("username not looked up and cached: %q %v", calls[2], kv)
	}

	// Too early: no call.
	if items, _ := x.Fetch(context.Background(), kv, now.Add(10*time.Minute)); items != nil || len(calls) != 3 {
		t.Fatalf("polled before min interval")
	}
	// Later: since_id used, budget capped at the daily remainder (60-1).
	if _, err := x.Fetch(context.Background(), kv, now.Add(31*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(calls[3], "since_id=900") || !strings.Contains(calls[3], "max_results=50") {
		t.Fatalf("bad follow-up query %q", calls[3])
	}
	// Cached name: no second lookup.
	if len(calls) != 4 {
		t.Fatalf("username looked up again: %q", calls)
	}
	// Budget used up: no call.
	kv[kvXReads+"2026-11-23"] = "58"
	kv[kvXLastPoll] = "0"
	if items, _ := x.Fetch(context.Background(), kv, now.Add(time.Hour)); items != nil || len(calls) != 4 {
		t.Fatalf("polled over budget")
	}
}

func TestXFetchMentionsAndAccounts(t *testing.T) {
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.URL.Path+"?"+r.URL.RawQuery)
		post := func(id, author, user string) string {
			return `{"data":[{"id":"` + id + `","text":"t","author_id":"` + author + `"}],
				"includes":{"users":[{"id":"` + author + `","username":"` + user + `"}]},"meta":{"newest_id":"` + id + `"}}`
		}
		switch r.URL.Path {
		case "/users/me":
			fmt.Fprint(w, `{"data":{"id":"42"}}`)
		case "/users/by":
			fmt.Fprint(w, `{"data":[{"id":"7","username":"ESRtweet"}]}`)
		case "/users/42/mentions":
			fmt.Fprint(w, post("100", "9", "fan"))
		case "/users/7/tweets":
			fmt.Fprint(w, post("200", "7", "ESRtweet"))
		default:
			http.Error(w, "unexpected", 500)
		}
	}))
	defer srv.Close()
	x := &X{Client: srv.Client(), BaseURL: srv.URL, Auth: OAuth1{"a", "b", "c", "d"},
		MaxPerPoll: 20, MaxPerDay: 100, MinInterval: time.Hour, Mentions: true, Accounts: []string{"esrtweet", "ghost"}}
	kv := memKV{}
	now := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	items, err := x.Fetch(context.Background(), kv, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].Priority != store.PriorityPing || items[1].Priority != store.PriorityDigest {
		t.Fatalf("bad items %+v", items)
	}
	// The account feed's author comes from the config; the mention's author
	// lookup fails here (500), and the billed post is kept with an id link.
	if items[1].URL != "https://x.com/esrtweet/status/200" || items[0].URL != "https://x.com/i/web/status/100" || items[0].Author != "" {
		t.Fatalf("bad authors %+v", items)
	}
	if strings.Contains(strings.Join(calls, "\n"), "ids=7") {
		t.Fatalf("account author looked up: %q", calls)
	}
	if !strings.Contains(calls[1], "start_time=2026-11-23T10") {
		t.Fatalf("first read must start 24h back: %q", calls[1])
	}
	if kv[kvXSince+"mentions"] != "100" || kv[kvXSince+"7"] != "200" || kv[kvXUID+"esrtweet"] != "7" || kv[kvXReads+"2026-11-24"] != "2" {
		t.Fatalf("state not saved: %v", kv)
	}
	// Next poll: cached id, no lookup; since ids used.
	calls = nil
	if _, err := x.Fetch(context.Background(), kv, now.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(calls, "\n")
	if strings.Contains(joined, "/users/by") || !strings.Contains(joined, "since_id=100") || !strings.Contains(joined, "since_id=200") {
		t.Fatalf("bad follow-up calls:\n%s", joined)
	}
}

func TestXFetchAllFeedsFail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/users/me" {
			fmt.Fprint(w, `{"data":{"id":"42"}}`)
			return
		}
		http.Error(w, `{"title":"Payment Required"}`, 402)
	}))
	defer srv.Close()
	x := &X{Client: srv.Client(), BaseURL: srv.URL, MaxPerPoll: 20, MaxPerDay: 100, MinInterval: time.Hour, Mentions: true, HomeTimeline: true}
	if _, err := x.Fetch(context.Background(), memKV{}, time.Now()); err == nil || !strings.Contains(err.Error(), "402") {
		t.Fatalf("want 402 error, got %v", err)
	}
}

func TestParseFeedLenient(t *testing.T) {
	cases := map[string]string{
		"html entity": `<rss><channel><item><title>A &rsquo; B</title><link>https://a.com/1</link></item></channel></rss>`,
		"latin1":      "<?xml version=\"1.0\" encoding=\"ISO-8859-1\"?><rss><channel><item><title>A \x92 B</title><link>https://a.com/1</link></item></channel></rss>",
	}
	for name, body := range cases {
		items, err := ParseFeed("f", []byte(body))
		if err != nil || len(items) != 1 || items[0].Title != "A ’ B" {
			t.Errorf("%s: got %+v, %v", name, items, err)
		}
	}
	if _, err := ParseFeed("f", []byte(`<?xml version="1.0" encoding="x-bogus"?><rss></rss>`)); err == nil {
		t.Error("unknown charset: want error")
	}
}

func TestPlain(t *testing.T) {
	cases := map[string]string{
		"<p>It&#8217;s &mdash; ok</p>":   "It’s — ok",
		"a&nbsp;&nbsp;b \n c":            "a b c",
		"AT&amp;T &lt;b&gt;not a tag":    "AT&T <b>not a tag",
		"<a href=\"x\">link</a>&#x27;s ": "link 's",
	}
	for in, want := range cases {
		if got := plain(in); got != want {
			t.Errorf("plain(%q) = %q, want %q", in, got, want)
		}
	}
}

type errKV struct{ memKV }

func (errKV) Get(context.Context, string) (string, error) { return "", errors.New("db down") }

func TestXFetchBudgetFailsClosed(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++ }))
	defer srv.Close()
	x := &X{Client: srv.Client(), BaseURL: srv.URL, MaxPerPoll: 10, MaxPerDay: 20, MinInterval: time.Hour, HomeTimeline: true}
	now := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)

	if _, err := x.Fetch(context.Background(), errKV{memKV{}}, now); err == nil || !strings.Contains(err.Error(), "db down") {
		t.Fatalf("kv error: got %v", err)
	}
	bad := memKV{kvXReads + "2026-11-24": "garbage"}
	if _, err := x.Fetch(context.Background(), bad, now); err == nil || !strings.Contains(err.Error(), "bad value") {
		t.Fatalf("corrupt counter: got %v", err)
	}
	if calls != 0 {
		t.Fatalf("X was called %d times despite an unreadable budget", calls)
	}
}

type recAlarms struct{ raised, cleared []string }

func (r *recAlarms) Raise(_ context.Context, key, _, _ string, _ time.Duration) {
	r.raised = append(r.raised, key)
}
func (r *recAlarms) Clear(_ context.Context, key string) { r.cleared = append(r.cleared, key) }

func TestXAlarms(t *testing.T) {
	status := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s := status[r.URL.Path]; s != 0 {
			http.Error(w, `{"title":"x"}`, s)
			return
		}
		switch r.URL.Path {
		case "/users/me":
			fmt.Fprint(w, `{"data":{"id":"42"}}`)
		case "/users/by":
			fmt.Fprint(w, `{"data":[{"id":"7","username":"prot"}]}`)
		default:
			fmt.Fprint(w, `{"data":[]}`)
		}
	}))
	defer srv.Close()
	rec := &recAlarms{}
	x := &X{Client: srv.Client(), BaseURL: srv.URL, MaxPerPoll: 10, MaxPerDay: 100, MinInterval: time.Hour,
		Mentions: true, Accounts: []string{"prot"}, Alarms: rec}
	kv := memKV{}
	now := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	poll := func() {
		now = now.Add(2 * time.Hour)
		rec.raised, rec.cleared = nil, nil
		x.Fetch(context.Background(), kv, now)
	}
	has := func(list []string, k string) bool { return slices.Contains(list, k) }

	status["/users/42/mentions"] = 402
	poll()
	if !has(rec.raised, AlarmXCredits) {
		t.Fatalf("402 must raise the credits alarm: %v", rec.raised)
	}
	status["/users/42/mentions"] = 0
	status["/users/7/tweets"] = 403 // protected account: not an auth problem
	poll()
	if has(rec.raised, AlarmXAuth) || !has(rec.cleared, AlarmXCredits) {
		t.Fatalf("raised %v cleared %v", rec.raised, rec.cleared)
	}
	status["/users/7/tweets"] = 0
	// One endpoint keeps failing while another works: raise, never clear in
	// the same poll, or the alarm would flap and repeat every poll.
	status["/users/42/mentions"] = 401
	for range 2 {
		poll()
		if !has(rec.raised, AlarmXAuth) || has(rec.cleared, AlarmXAuth) {
			t.Fatalf("401 next to a working feed: raised %v cleared %v", rec.raised, rec.cleared)
		}
	}
	status["/users/42/mentions"] = 0
	poll()
	if !has(rec.cleared, AlarmXAuth) || has(rec.raised, AlarmXAuth) {
		t.Fatalf("all feeds fine must clear the auth alarm: raised %v cleared %v", rec.raised, rec.cleared)
	}
	kv[kvXReads+now.Add(2*time.Hour).UTC().Format("2006-01-02")] = "98"
	poll()
	if !has(rec.raised, AlarmXDailyCap) {
		t.Fatalf("used-up budget must raise the daily cap alarm: %v", rec.raised)
	}
	now = now.Add(24 * time.Hour) // next UTC day
	poll()
	if !has(rec.cleared, AlarmXDailyCap) {
		t.Fatalf("new day must clear the daily cap alarm: %v", rec.cleared)
	}
}

// lastPollFailKV fails only when saving the last poll time.
type lastPollFailKV struct{ memKV }

func (k lastPollFailKV) Set(ctx context.Context, key, v string) error {
	if key == kvXLastPoll {
		return errors.New("disk full")
	}
	return k.memKV.Set(ctx, key, v)
}

func TestXFetchKeepsItemsWhenLastPollSaveFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users/me":
			fmt.Fprint(w, `{"data":{"id":"42"}}`)
		default:
			fmt.Fprint(w, `{"data":[{"id":"900","text":"hi","author_id":"1","created_at":"2026-11-24T09:00:00Z"}],"includes":{"users":[{"id":"1","username":"bob"}]},"meta":{"newest_id":"900"}}`)
		}
	}))
	defer srv.Close()
	x := &X{Client: srv.Client(), BaseURL: srv.URL, MaxPerPoll: 10, MaxPerDay: 100, MinInterval: time.Hour, HomeTimeline: true}
	kv := lastPollFailKV{memKV{}}
	items, err := x.Fetch(context.Background(), kv, time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC))
	if err != nil || len(items) != 1 {
		t.Fatalf("billed posts dropped: items=%v err=%v", items, err)
	}
	if kv.memKV[kvXSinceID] != "900" {
		t.Fatalf("since id not saved: %v", kv.memKV)
	}
}

func TestXUsernamesCache(t *testing.T) {
	var lookups []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lookups = append(lookups, r.URL.Query().Get("ids"))
		fmt.Fprint(w, `{"data":[{"id":"1","username":"new_name"},{"id":"3","username":"carol"}]}`)
	}))
	defer srv.Close()
	x := &X{Client: srv.Client(), BaseURL: srv.URL}
	now := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	kv := memKV{
		kvXName + "1": fmt.Sprint("old_name ", now.Add(-nameMaxAge-time.Hour).Unix()), // stale
		kvXName + "2": fmt.Sprint("bob ", now.Add(-time.Hour).Unix()),                 // fresh
	}
	names := x.usernames(context.Background(), kv, []string{"1", "2", "3", "2"}, now)
	if names["1"] != "new_name" || names["2"] != "bob" || names["3"] != "carol" {
		t.Fatalf("names %v", names)
	}
	if len(lookups) != 1 || lookups[0] != "1,3" {
		t.Fatalf("want one lookup of the stale and unknown ids, got %q", lookups)
	}
	if !strings.HasPrefix(kv[kvXName+"1"], "new_name ") || !strings.HasPrefix(kv[kvXName+"3"], "carol ") {
		t.Fatalf("cache not updated: %v", kv)
	}
	// All fresh now: no request.
	x.usernames(context.Background(), kv, []string{"1", "2", "3"}, now)
	if len(lookups) != 1 {
		t.Fatalf("fresh names looked up again: %q", lookups)
	}
}

func TestXFetchRepostStoredAsOriginal(t *testing.T) {
	var query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users/me":
			fmt.Fprint(w, `{"data":{"id":"42"}}`)
		case "/users":
			fmt.Fprint(w, `{"data":[{"id":"7","username":"hdmoore"}]}`)
		default:
			query = r.URL.RawQuery
			fmt.Fprint(w, `{"data":[
				{"id":"9001","text":"RT @BleepinComputer: OpenRouter drops AI providers &amp; more…","author_id":"7","created_at":"2026-11-24T08:00:00Z",
				 "referenced_tweets":[{"type":"retweeted","id":"8001"}]},
				{"id":"9002","text":"own post","author_id":"7","created_at":"2026-11-24T08:01:00Z",
				 "referenced_tweets":[{"type":"quoted","id":"8002"}]}],
				"meta":{"newest_id":"9002"}}`)
		}
	}))
	defer srv.Close()
	x := &X{Client: srv.Client(), BaseURL: srv.URL, MaxPerPoll: 10, MaxPerDay: 100, MinInterval: time.Hour, HomeTimeline: true}
	items, err := x.Fetch(context.Background(), memKV{}, time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC))
	if err != nil || len(items) != 2 {
		t.Fatal(items, err)
	}
	if !strings.Contains(query, "referenced_tweets") || strings.Contains(query, "expansions") {
		t.Fatalf("bad query %q", query)
	}
	rt := items[0]
	if rt.ExtID != "8001" || rt.URL != "https://x.com/BleepinComputer/status/8001" ||
		rt.Title != "@hdmoore reposted @BleepinComputer: OpenRouter drops AI providers & more…" || rt.Author != "hdmoore" ||
		!rt.Published.Equal(time.Date(2026, 11, 24, 8, 0, 0, 0, time.UTC)) {
		t.Fatalf("bad repost %+v", rt)
	}
	// A quote is the quoting account's own post.
	if items[1].ExtID != "9002" || items[1].Title != "@hdmoore: own post" {
		t.Fatalf("bad quote %+v", items[1])
	}
}
