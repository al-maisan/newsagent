package sources

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/al-maisan/newsagent/internal/store"
)

// Reddit's RSS feeds were shut down in November 2026; only the official
// OAuth API remains, so Reddit goes through it (app-only: a script app's
// id and secret, no user login).
const (
	RedditBaseURL  = "https://oauth.reddit.com"
	RedditTokenURL = "https://www.reddit.com/api/v1/access_token"
)

// AlarmRedditAuth is the alarm key of the Reddit source.
const AlarmRedditAuth = "reddit_auth"

// Reddit reads the hot pages of the configured subreddits.
type Reddit struct {
	Client       *http.Client
	BaseURL      string
	TokenURL     string
	ClientID     string
	ClientSecret string
	Subreddits   []string
	TopN         int
	Alarms       Alarms // optional

	bearer string    // fetched once per Fetch call
	seen   *authSeen // outcomes of the current Fetch
}

// authSeen collects request outcomes during one Fetch. Alarms are decided
// once at its end: one failing subreddit next to working ones must not
// raise and clear the same alarm on every poll.
type authSeen struct {
	ok         bool // some request succeeded
	authStatus int  // 401 from the API
	authBody   string
}

// Fetch returns the hot posts of every subreddit. One failing subreddit is
// logged and skipped; an error is returned only if every request failed.
func (r *Reddit) Fetch(ctx context.Context) ([]store.Item, error) {
	r.seen = &authSeen{}
	defer r.settleAlarms(ctx)
	tok, err := r.token(ctx)
	if err != nil {
		return nil, err
	}
	r.bearer = tok
	var items []store.Item
	var errs []error
	for _, sub := range r.Subreddits {
		got, err := r.hot(ctx, sub)
		if err != nil {
			slog.Warn("reddit read failed", "subreddit", sub, "err", err)
			errs = append(errs, fmt.Errorf("r/%s: %w", sub, err))
			continue
		}
		items = append(items, got...)
	}
	if len(items) == 0 && len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return items, nil
}

// token fetches an app-only OAuth token. Reddit allows roughly one request
// per second, so the token is fetched once per run, not per subreddit; it
// expires long before the next poll.
func (r *Reddit) token(ctx context.Context) (string, error) {
	form := url.Values{"grant_type": {"client_credentials"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(r.ClientID, r.ClientSecret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", userAgent)
	resp, err := r.Client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return "", fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		e := &HTTPError{Status: resp.StatusCode, Body: clip(string(body), 200)}
		if s := r.seen; s != nil && e.Status == http.StatusUnauthorized {
			// A wrong client id or secret. A 403 is not an auth problem:
			// it means a blocked user agent or a private subreddit.
			s.authStatus, s.authBody = e.Status, e.Body
		}
		return "", e
	}
	var out struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("decode token: %w", err)
	}
	if out.AccessToken == "" {
		return "", errors.New("reddit token: empty access_token")
	}
	if s := r.seen; s != nil {
		s.ok = true
	}
	return out.AccessToken, nil
}

type listing struct {
	Data struct {
		Children []struct {
			Kind string `json:"kind"`
			Data post   `json:"data"`
		} `json:"children"`
	} `json:"data"`
}

type post struct {
	ID          string  `json:"id"`
	Title       string  `json:"title"`
	URL         string  `json:"url"`
	Permalink   string  `json:"permalink"`
	Author      string  `json:"author"`
	Selftext    string  `json:"selftext"`
	CreatedUTC  float64 `json:"created_utc"`
	Score       int     `json:"score"`
	NumComments int     `json:"num_comments"`
	Stickied    bool    `json:"stickied"`
	Over18      bool    `json:"over_18"`
}

func (r *Reddit) hot(ctx context.Context, sub string) ([]store.Item, error) {
	q := url.Values{"limit": {strconv.Itoa(r.TopN)}, "raw_json": {"1"}}
	u := r.BaseURL + "/r/" + url.PathEscape(sub) + "/hot?" + q.Encode()
	hdr := http.Header{"Authorization": {"Bearer " + r.bearer}}
	var list listing
	if err := r.getJSON(ctx, u, hdr, &list); err != nil {
		return nil, err
	}
	var out []store.Item
	for _, c := range list.Data.Children {
		p := c.Data
		if p.ID == "" || p.Stickied || p.Over18 {
			continue
		}
		perm := "https://www.reddit.com" + p.Permalink
		itemURL := p.URL
		if !isWebURL(itemURL) || strings.EqualFold(itemURL, perm) {
			itemURL = perm // a self post's url is its own permalink
		}
		text := fmt.Sprintf("%d points, %d comments", p.Score, p.NumComments)
		if t := plain(p.Selftext); t != "" {
			text += " · " + clip(t, maxTextRunes)
		}
		out = append(out, store.Item{
			Source:     "reddit",
			ExtID:      p.ID,
			Title:      clip(plain(p.Title), 300),
			URL:        itemURL,
			DiscussURL: perm,
			Author:     clip(plain(p.Author), 100),
			Text:       text,
			Published:  time.Unix(int64(p.CreatedUTC), 0).UTC(),
		})
	}
	return out, nil
}

func (r *Reddit) getJSON(ctx context.Context, u string, hdr http.Header, v any) error {
	err := getJSON(ctx, r.Client, u, hdr, v)
	if s := r.seen; s != nil {
		var he *HTTPError
		switch {
		case err == nil:
			s.ok = true
		case errors.As(err, &he) && he.Status == http.StatusUnauthorized:
			s.authStatus, s.authBody = he.Status, he.Body
		}
	}
	return err
}

// settleAlarms raises or clears the auth alarm from what this Fetch saw. A
// Fetch without a successful request changes nothing.
func (r *Reddit) settleAlarms(ctx context.Context) {
	s := r.seen
	r.seen = nil
	if s.authStatus != 0 {
		r.raise(ctx, AlarmRedditAuth, "Reddit API access denied",
			fmt.Sprintf("Reddit answered %d: check REDDIT_CLIENT_ID and REDDIT_CLIENT_SECRET in /etc/newsagent/secrets.env and the app on reddit.com/prefs/apps (type \"script\"). Details: %s", s.authStatus, s.authBody), alarmRepeat)
	} else if s.ok {
		r.clear(ctx, AlarmRedditAuth)
	}
}

func (r *Reddit) raise(ctx context.Context, key, title, detail string, repeat time.Duration) {
	if r.Alarms != nil {
		r.Alarms.Raise(ctx, key, title, detail, repeat)
	}
}

func (r *Reddit) clear(ctx context.Context, key string) {
	if r.Alarms != nil {
		r.Alarms.Clear(ctx, key)
	}
}
