package sources

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/al-maisan/newsagent/internal/store"
)

// XBaseURL is the X API v2 base URL.
const XBaseURL = "https://api.x.com/2"

// KV is the state the X source keeps between runs.
type KV interface {
	Get(ctx context.Context, key string) (string, error)
	Set(ctx context.Context, key, value string) error
	DeleteBefore(ctx context.Context, prefix, bound string) error
}

// X reads, in this order: mentions of the authenticated user, new posts of
// selected accounts, and the home timeline. X bills per post read, so reads
// are capped per request and per UTC day; the order decides what gets the
// budget first.
type X struct {
	Client         *http.Client
	BaseURL        string
	Auth           OAuth1
	MaxPerPoll     int // per request
	MaxPerDay      int
	MinInterval    time.Duration
	ExcludeReplies bool
	Mentions       bool
	Accounts       []string // usernames whose posts always reach the digest
	HomeTimeline   bool
}

const (
	kvXUserID   = "x_user_id"
	kvXSinceID  = "x_since_id" // home timeline
	kvXLastPoll = "x_last_poll"
	kvXReads    = "x_reads:"     // + YYYY-MM-DD (UTC)
	kvXSince    = "x_since:"     // + "mentions" or user id
	kvXUID      = "x_uid:"       // + lower-case username
	firstLook   = 24 * time.Hour // how far back the first read of a feed goes
	notFound    = "-"            // cached id of a username X does not know
)

type xTimeline struct {
	Data []struct {
		ID            string `json:"id"`
		Text          string `json:"text"`
		AuthorID      string `json:"author_id"`
		CreatedAt     string `json:"created_at"`
		PublicMetrics struct {
			Likes   int `json:"like_count"`
			Reposts int `json:"retweet_count"`
			Replies int `json:"reply_count"`
		} `json:"public_metrics"`
	} `json:"data"`
	Includes struct {
		Users []struct {
			ID       string `json:"id"`
			Username string `json:"username"`
		} `json:"users"`
	} `json:"includes"`
	Meta struct {
		NewestID string `json:"newest_id"`
	} `json:"meta"`
}

// budget tracks the posts read today; it is saved after every request.
type budget struct {
	kv        KV
	key       string
	used, max int
}

func (b *budget) left() int { return b.max - b.used }

func (b *budget) spend(ctx context.Context, n int) error {
	b.used += n
	return b.kv.Set(ctx, b.key, strconv.Itoa(b.used))
}

// Fetch returns new posts, or nothing if it is too early or the daily budget
// is used up. Failing feeds are logged and skipped; an error is returned only
// if every feed failed.
func (x *X) Fetch(ctx context.Context, kv KV, now time.Time) ([]store.Item, error) {
	last, err := kv.Get(ctx, kvXLastPoll)
	if err != nil {
		return nil, fmt.Errorf("x last poll: %w", err)
	}
	if last != "" {
		// A bad value only costs one early read; the daily budget still holds.
		if ts, err := strconv.ParseInt(last, 10, 64); err != nil {
			slog.Warn("x last poll unreadable, ignoring it", "value", last)
		} else if now.Sub(time.Unix(ts, 0)) < x.MinInterval {
			return nil, nil
		}
	}
	// The read counter guards paid reads: if it can't be read, read nothing.
	b := &budget{kv: kv, key: kvXReads + now.UTC().Format("2006-01-02"), max: x.MaxPerDay}
	if err := kv.DeleteBefore(ctx, kvXReads, b.key); err != nil {
		slog.Warn("x: dropping old read counters failed", "err", err)
	}
	v, err := kv.Get(ctx, b.key)
	if err != nil {
		return nil, fmt.Errorf("x read budget: %w", err)
	}
	if v != "" {
		if b.used, err = strconv.Atoi(v); err != nil || b.used < 0 {
			return nil, fmt.Errorf("x read budget: bad value %q in %s", v, b.key)
		}
	}
	if b.left() < 5 { // the API's minimum max_results is 5
		slog.Info("x daily read budget used up", "used", b.used, "max", x.MaxPerDay)
		return nil, nil
	}
	uid, err := x.userID(ctx, kv)
	if err != nil {
		return nil, err
	}

	var items []store.Item
	var errs []error
	read := func(name, path, sinceKey, priority string, excludeReplies bool) {
		got, err := x.read(ctx, kv, b, now, path, sinceKey, priority, excludeReplies)
		if err != nil {
			slog.Warn("x read failed", "feed", name, "err", err)
			errs = append(errs, fmt.Errorf("x %s: %w", name, err))
			return
		}
		items = append(items, got...)
	}
	if x.Mentions {
		read("mentions", "/users/"+uid+"/mentions", kvXSince+"mentions", store.PriorityPing, false)
	}
	if len(x.Accounts) > 0 {
		ids, err := x.accountIDs(ctx, kv)
		if err != nil {
			errs = append(errs, err)
		}
		for _, a := range x.Accounts {
			if id := ids[strings.ToLower(a)]; id != "" {
				read("@"+a, "/users/"+id+"/tweets", kvXSince+id, store.PriorityDigest, x.ExcludeReplies)
			}
		}
	}
	if x.HomeTimeline {
		read("home", "/users/"+uid+"/timelines/reverse_chronological", kvXSinceID, "", x.ExcludeReplies)
	}
	if err := kv.Set(ctx, kvXLastPoll, strconv.FormatInt(now.Unix(), 10)); err != nil {
		return nil, err
	}
	if len(items) == 0 && len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return items, nil
}

// read fetches one feed's posts newer than the stored since id. Spend and
// progress are saved before returning, so billed posts are never read twice.
func (x *X) read(ctx context.Context, kv KV, b *budget, now time.Time, path, sinceKey, priority string, excludeReplies bool) ([]store.Item, error) {
	n := min(x.MaxPerPoll, b.left(), 100)
	if n < 5 {
		return nil, nil
	}
	q := url.Values{
		"max_results": {strconv.Itoa(n)},
		"post.fields": {"created_at,author_id,public_metrics"},
		"expansions":  {"author_id"},
		"user.fields": {"username"},
	}
	if excludeReplies {
		q.Set("exclude", "replies")
	}
	since, err := kv.Get(ctx, sinceKey)
	if err != nil {
		return nil, err
	}
	if since != "" {
		q.Set("since_id", since)
	} else {
		q.Set("start_time", now.Add(-firstLook).UTC().Format(time.RFC3339))
	}
	var tl xTimeline
	if err := x.get(ctx, path, q, &tl); err != nil {
		return nil, err
	}
	if err := b.spend(ctx, len(tl.Data)); err != nil {
		return nil, err
	}
	if tl.Meta.NewestID != "" {
		if err := kv.Set(ctx, sinceKey, tl.Meta.NewestID); err != nil {
			return nil, err
		}
	}

	users := map[string]string{}
	for _, u := range tl.Includes.Users {
		users[u.ID] = u.Username
	}
	var out []store.Item
	for _, p := range tl.Data {
		name := users[p.AuthorID]
		if name == "" || p.ID == "" {
			continue
		}
		text := plain(p.Text)
		out = append(out, store.Item{
			Source:    "x",
			ExtID:     p.ID,
			Title:     clip("@"+name+": "+text, 300),
			URL:       "https://x.com/" + url.PathEscape(name) + "/status/" + url.PathEscape(p.ID),
			Author:    name,
			Text:      fmt.Sprintf("%s (likes %d, reposts %d, replies %d)", clip(text, maxTextRunes), p.PublicMetrics.Likes, p.PublicMetrics.Reposts, p.PublicMetrics.Replies),
			Published: parseTime(p.CreatedAt),
			Priority:  priority,
		})
	}
	return out, nil
}

// accountIDs maps lower-case usernames to user ids. Ids are cached; new
// usernames are looked up in one batched request. Usernames X does not know
// are cached as notFound, so they cost one lookup only.
func (x *X) accountIDs(ctx context.Context, kv KV) (map[string]string, error) {
	ids := map[string]string{}
	var missing []string
	for _, a := range x.Accounts {
		key := strings.ToLower(a)
		id, err := kv.Get(ctx, kvXUID+key)
		if err != nil {
			return ids, err
		}
		if id == notFound {
			continue
		}
		if id != "" {
			ids[key] = id
		} else {
			missing = append(missing, a)
		}
	}
	if len(missing) == 0 {
		return ids, nil
	}
	var res struct {
		Data []struct {
			ID       string `json:"id"`
			Username string `json:"username"`
		} `json:"data"`
	}
	if err := x.get(ctx, "/users/by", url.Values{"usernames": {strings.Join(missing, ",")}}, &res); err != nil {
		return ids, fmt.Errorf("x users/by: %w", err)
	}
	for _, u := range res.Data {
		key := strings.ToLower(u.Username)
		if u.ID == "" {
			continue
		}
		if err := kv.Set(ctx, kvXUID+key, u.ID); err != nil {
			return ids, err
		}
		ids[key] = u.ID
	}
	for _, a := range missing {
		if key := strings.ToLower(a); ids[key] == "" {
			slog.Warn("x account not found, skipping it", "username", a)
			if err := kv.Set(ctx, kvXUID+key, notFound); err != nil {
				return ids, err
			}
		}
	}
	return ids, nil
}

// userID returns the authenticated user's id, looked up once and cached.
func (x *X) userID(ctx context.Context, kv KV) (string, error) {
	if id, err := kv.Get(ctx, kvXUserID); err != nil || id != "" {
		return id, err
	}
	var me struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := x.get(ctx, "/users/me", nil, &me); err != nil {
		return "", fmt.Errorf("x users/me: %w", err)
	}
	if me.Data.ID == "" {
		return "", errors.New("x users/me: empty id")
	}
	return me.Data.ID, kv.Set(ctx, kvXUserID, me.Data.ID)
}

func (x *X) get(ctx context.Context, path string, q url.Values, v any) error {
	base := x.BaseURL + path
	u := base
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	hdr := http.Header{"Authorization": {x.Auth.header(http.MethodGet, base, q)}}
	return getJSON(ctx, x.Client, u, hdr, v)
}
