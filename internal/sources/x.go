package sources

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/al-maisan/newsagent/internal/store"
)

// XBaseURL is the X API v2 base URL.
const XBaseURL = "https://api.x.com/2"

// KV is the state the X source keeps between runs.
type KV interface {
	Get(ctx context.Context, key string) (string, error)
	Set(ctx context.Context, key, value string) error
}

// X reads the authenticated user's home timeline (reverse chronological).
// X bills per post read, so reads are capped per poll and per UTC day.
type X struct {
	Client         *http.Client
	BaseURL        string
	Auth           OAuth1
	MaxPerPoll     int
	MaxPerDay      int
	MinInterval    time.Duration
	ExcludeReplies bool
}

const (
	kvXUserID   = "x_user_id"
	kvXSinceID  = "x_since_id"
	kvXLastPoll = "x_last_poll"
	kvXReads    = "x_reads:" // + YYYY-MM-DD (UTC)
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
		NewestID    string `json:"newest_id"`
		ResultCount int    `json:"result_count"`
	} `json:"meta"`
}

// Fetch returns new timeline posts, or nothing if it is too early or the
// daily budget is used up.
func (x *X) Fetch(ctx context.Context, kv KV, now time.Time) ([]store.Item, error) {
	if last, _ := kv.Get(ctx, kvXLastPoll); last != "" {
		if ts, err := strconv.ParseInt(last, 10, 64); err == nil && now.Sub(time.Unix(ts, 0)) < x.MinInterval {
			return nil, nil
		}
	}
	readsKey := kvXReads + now.UTC().Format("2006-01-02")
	used := 0
	if v, _ := kv.Get(ctx, readsKey); v != "" {
		used, _ = strconv.Atoi(v)
	}
	budget := min(x.MaxPerPoll, x.MaxPerDay-used, 100)
	if budget < 5 { // the API's minimum max_results is 5
		slog.Info("x daily read budget used up", "used", used, "max", x.MaxPerDay)
		return nil, nil
	}

	uid, err := x.userID(ctx, kv)
	if err != nil {
		return nil, err
	}
	q := url.Values{
		"max_results": {strconv.Itoa(budget)},
		"post.fields": {"created_at,author_id,public_metrics"},
		"expansions":  {"author_id"},
		"user.fields": {"username"},
	}
	if x.ExcludeReplies {
		q.Set("exclude", "replies")
	}
	since, err := kv.Get(ctx, kvXSinceID)
	if err != nil {
		return nil, err
	}
	if since != "" {
		q.Set("since_id", since)
	}
	var tl xTimeline
	if err := x.get(ctx, "/users/"+uid+"/timelines/reverse_chronological", q, &tl); err != nil {
		return nil, fmt.Errorf("x timeline: %w", err)
	}
	// Record spend and progress before returning items, so a later failure
	// never causes the same (billed) posts to be read again.
	if err := kv.Set(ctx, readsKey, strconv.Itoa(used+len(tl.Data))); err != nil {
		return nil, err
	}
	if tl.Meta.NewestID != "" {
		if err := kv.Set(ctx, kvXSinceID, tl.Meta.NewestID); err != nil {
			return nil, err
		}
	}
	if err := kv.Set(ctx, kvXLastPoll, strconv.FormatInt(now.Unix(), 10)); err != nil {
		return nil, err
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
		})
	}
	return out, nil
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
