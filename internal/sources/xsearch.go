package sources

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/al-maisan/newsagent/internal/store"
)

// XSearchBaseURL is the twitterapi.io API, an unofficial X data service.
const XSearchBaseURL = "https://api.twitterapi.io"

// XSearch reads chosen X accounts through twitterapi.io's advanced search,
// billed per tweet returned ($0.15 per 1,000) with a 15-credit ($0.00015)
// minimum per call. One query covers many accounts, and since_id limits each
// poll to posts not seen yet.
type XSearch struct {
	Client         *http.Client
	BaseURL        string
	Key            string // TWITTERAPI_IO_KEY
	Accounts       []string
	AlwaysDigest   []string // accounts whose posts always reach the digest
	ExcludeReplies bool
	IncludeReposts bool // plain reposts (native retweets) of the accounts
	MinInterval    time.Duration
	MaxPerDay      int           // safety cap on tweets read per UTC day
	Pause          time.Duration // between requests (free tier: 1 per 5s)
	Alarms         Alarms        // optional
}

// Alarm keys of the twitterapi.io source.
const (
	AlarmXSearchCredits = "xsearch_credits"
	AlarmXSearchAuth    = "xsearch_auth"
)

const (
	kvXSLastPoll = "xs_last_poll"
	kvXSReads    = "xs_reads:" // + YYYY-MM-DD (UTC)
	kvXSSince    = "xs_since:" // + hash of the query's accounts
	xsMaxQuery   = 512         // the API's query length limit
	xsMaxPages   = 5           // per query and poll (20 tweets each)
)

type xsPost struct {
	ID           string `json:"id"`
	Text         string `json:"text"`
	CreatedAt    string `json:"createdAt"`
	LikeCount    int    `json:"likeCount"`
	RetweetCount int    `json:"retweetCount"`
	ReplyCount   int    `json:"replyCount"`
	Author       struct {
		UserName string `json:"userName"`
	} `json:"author"`
}

type xsPage struct {
	Tweets []struct {
		xsPost
		IsRetweet bool    `json:"isRetweet"`
		Original  *xsPost `json:"retweeted_tweet"` // set for reposts
	} `json:"tweets"`
	HasNextPage bool   `json:"has_next_page"`
	NextCursor  string `json:"next_cursor"`
}

// xsQuery is one search over a group of accounts.
type xsQuery struct {
	key  string // stable id of the account group, for its since_id
	text string // "(from:a OR from:b) [-filter:replies]"
}

// Fetch returns new posts of the configured accounts, or nothing if it is too
// early or today's cap is used up. A failing query is logged and skipped; an
// error is returned only if nothing could be read.
func (x *XSearch) Fetch(ctx context.Context, kv KV, now time.Time) ([]store.Item, error) {
	last, err := kv.Get(ctx, kvXSLastPoll)
	if err != nil {
		return nil, fmt.Errorf("xsearch last poll: %w", err)
	}
	if ts, err := strconv.ParseInt(last, 10, 64); err == nil && now.Sub(time.Unix(ts, 0)) < x.MinInterval {
		return nil, nil
	}
	b := &budget{kv: kv, key: kvXSReads + now.UTC().Format("2006-01-02"), max: x.MaxPerDay}
	if err := kv.DeleteBefore(ctx, kvXSReads, b.key); err != nil {
		slog.Warn("xsearch: dropping old read counters failed", "err", err)
	}
	v, err := kv.Get(ctx, b.key)
	if err != nil {
		return nil, fmt.Errorf("xsearch read budget: %w", err)
	}
	if v != "" {
		if b.used, err = strconv.Atoi(v); err != nil || b.used < 0 {
			return nil, fmt.Errorf("xsearch read budget: bad value %q in %s", v, b.key)
		}
	}
	if b.left() <= 0 {
		slog.Info("xsearch daily cap reached", "used", b.used, "max", x.MaxPerDay)
		return nil, nil
	}

	var items []store.Item
	var errs []error
	var credits, ok bool
	var authStatus int
	for i, q := range x.queries() {
		if i > 0 && !x.pause(ctx) {
			break
		}
		got, err := x.search(ctx, kv, b, q, now)
		items = append(items, got...)
		var he *HTTPError
		switch {
		case err == nil:
			ok = true
		case errors.As(err, &he) && he.Status == http.StatusPaymentRequired:
			credits = true
		case errors.As(err, &he) && (he.Status == http.StatusUnauthorized || he.Status == http.StatusForbidden):
			authStatus = he.Status
		}
		if err != nil {
			slog.Warn("xsearch query failed", "query", i, "err", err)
			errs = append(errs, err)
		}
	}
	// Decided once per poll, so one failing query next to working ones
	// can't raise and clear the same alarm on every poll.
	if credits {
		x.raise(ctx, AlarmXSearchCredits, "twitterapi.io credits used up",
			"twitterapi.io answered 402: no X posts until you top up credits at twitterapi.io/dashboard.")
	} else if ok {
		x.clear(ctx, AlarmXSearchCredits)
	}
	if authStatus != 0 {
		x.raise(ctx, AlarmXSearchAuth, "twitterapi.io access denied",
			fmt.Sprintf("twitterapi.io answered %d: check TWITTERAPI_IO_KEY in /etc/newsagent/secrets.env.", authStatus))
	} else if ok {
		x.clear(ctx, AlarmXSearchAuth)
	}
	if err := kv.Set(ctx, kvXSLastPoll, strconv.FormatInt(now.Unix(), 10)); err != nil {
		slog.Warn("xsearch: saving last poll time failed", "err", err)
	}
	if len(items) == 0 && len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return items, nil
}

// queries packs the accounts into as few searches as fit the API's query
// length, leaving room for since_id/since_time.
func (x *XSearch) queries() []xsQuery {
	filter := ""
	if x.ExcludeReplies {
		filter = " -filter:replies"
	}
	if x.IncludeReposts {
		filter += " include:nativeretweets"
	}
	room := xsMaxQuery - len(filter) - len(" since_time:") - 20 // longest since suffix
	var out []xsQuery
	var group []string
	flush := func() {
		if len(group) == 0 {
			return
		}
		sorted := append([]string(nil), group...)
		sort.Strings(sorted)
		sum := sha1.Sum([]byte(strings.Join(sorted, ",")))
		out = append(out, xsQuery{key: hex.EncodeToString(sum[:8]), text: "(from:" + strings.Join(group, " OR from:") + ")" + filter})
		group = nil
	}
	for _, a := range x.Accounts {
		a = strings.ToLower(a)
		if len(group) > 0 && len("(from:"+strings.Join(append(group, a), " OR from:")+")") > room {
			flush()
		}
		group = append(group, a)
	}
	flush()
	return out
}

// search runs one query, following pages up to xsMaxPages. since_id moves
// forward only when every page was read; after an error the next poll asks
// again (the store dedupes what was already returned).
func (x *XSearch) search(ctx context.Context, kv KV, b *budget, q xsQuery, now time.Time) ([]store.Item, error) {
	since, err := kv.Get(ctx, kvXSSince+q.key)
	if err != nil {
		return nil, err
	}
	query := q.text + " since_time:" + strconv.FormatInt(now.Add(-firstLook).Unix(), 10)
	if since != "" {
		query = q.text + " since_id:" + since
	}
	digest := map[string]bool{}
	for _, a := range x.AlwaysDigest {
		digest[strings.ToLower(a)] = true
	}
	newest := since
	var out []store.Item
	cursor, more, pages := "", false, 0
	for ; pages < xsMaxPages && b.left() > 0; pages++ {
		if pages > 0 && !x.pause(ctx) {
			return out, ctx.Err()
		}
		var p xsPage
		vals := url.Values{"query": {query}, "queryType": {"Latest"}, "cursor": {cursor}}
		u := x.BaseURL + "/twitter/tweet/advanced_search?" + vals.Encode()
		if err := getJSON(ctx, x.Client, u, http.Header{"X-API-Key": {x.Key}}, &p); err != nil {
			return out, err
		}
		if err := b.spend(ctx, len(p.Tweets)); err != nil {
			return out, err
		}
		for _, t := range p.Tweets {
			if t.ID == "" || t.Author.UserName == "" {
				continue
			}
			if newerID(t.ID, newest) {
				newest = t.ID
			}
			it := xsItem(t.xsPost, nil)
			if o := t.Original; t.IsRetweet && o != nil && o.ID != "" && o.Author.UserName != "" {
				it = xsItem(t.xsPost, o)
			}
			if digest[strings.ToLower(t.Author.UserName)] {
				it.Priority = store.PriorityDigest
			}
			out = append(out, it)
		}
		more = p.HasNextPage && p.NextCursor != ""
		if !more {
			break
		}
		cursor = p.NextCursor
	}
	// since_id moves past what is left; Latest is newest first, so those
	// posts can't be fetched later. Say so.
	if more {
		slog.Warn("xsearch: page or daily cap reached, older new posts skipped", "query", q.key, "pages", pages, "left_today", b.left())
	}
	if newest != since {
		if err := kv.Set(ctx, kvXSSince+q.key, newest); err != nil {
			return out, err
		}
	}
	return out, nil
}

// xsItem builds the item for t. For a repost (orig set) the item is the
// original post: its id (so a story reposted by several accounts is stored
// once), link and full text, as "@reposter reposted @author: …". Author and
// date stay the reposter's, so always_digest and max_age apply to the repost.
func xsItem(t xsPost, orig *xsPost) store.Item {
	pub, _ := time.Parse(time.RubyDate, t.CreatedAt)
	post, prefix := t, "@"+t.Author.UserName+": "
	if orig != nil {
		post, prefix = *orig, "@"+t.Author.UserName+" reposted @"+orig.Author.UserName+": "
	}
	text := plain(post.Text)
	return store.Item{
		Source:    "x",
		ExtID:     post.ID,
		Title:     clip(prefix+text, 300),
		URL:       "https://x.com/" + url.PathEscape(post.Author.UserName) + "/status/" + url.PathEscape(post.ID),
		Author:    t.Author.UserName,
		Text:      fmt.Sprintf("%s (likes %d, reposts %d, replies %d)", clip(text, maxTextRunes), post.LikeCount, post.RetweetCount, post.ReplyCount),
		Published: pub.UTC(),
	}
}

// newerID reports whether post id a is newer than b (ids are increasing
// decimal numbers; "" is older than everything).
func newerID(a, b string) bool {
	if len(a) != len(b) {
		return len(a) > len(b)
	}
	return a > b
}

// pause waits x.Pause, or returns false if ctx ends first.
func (x *XSearch) pause(ctx context.Context) bool {
	if x.Pause <= 0 {
		return true
	}
	t := time.NewTimer(x.Pause)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func (x *XSearch) raise(ctx context.Context, key, title, detail string) {
	if x.Alarms != nil {
		x.Alarms.Raise(ctx, key, title, detail, 6*time.Hour)
	}
}

func (x *XSearch) clear(ctx context.Context, key string) {
	if x.Alarms != nil {
		x.Alarms.Clear(ctx, key)
	}
}
