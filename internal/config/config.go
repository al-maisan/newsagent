// Package config loads the JSON config file and secrets from the environment.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/al-maisan/newsagent/internal/store"
)

// Duration is a time.Duration that reads from JSON strings like "30m".
type Duration time.Duration

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string like \"30m\": %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// Feed is one RSS/Atom source.
type Feed struct {
	Name string `json:"name"`
	URL  string `json:"url"`
	// Private lets newsagent connect to this feed's host on a private
	// address (LAN, tailnet). Its DNS is then trusted: only set it for
	// hosts you control.
	Private bool `json:"private"`
}

// PrivateHosts returns the hosts that may resolve to private addresses:
// feeds marked private, and feeds whose host is an IP literal (no DNS, so
// nothing to rebind). Every other host is checked on each connection.
func (c *Config) PrivateHosts() []string {
	var hosts []string
	for _, f := range c.Feeds {
		u, err := url.Parse(f.URL)
		if err != nil {
			continue
		}
		if h := u.Hostname(); f.Private || net.ParseIP(h) != nil {
			hosts = append(hosts, h)
		}
	}
	return hosts
}

// Config is the newsagent configuration.
type Config struct {
	DBPath   string   `json:"db_path"`
	MaxAge   Duration `json:"max_age"`  // ignore items published earlier than this
	Timezone string   `json:"timezone"` // for times shown in messages
	Feeds    []Feed   `json:"feeds"`
	HN       struct {
		Enabled bool `json:"enabled"`
		TopN    int  `json:"top_n"`
	} `json:"hn"`
	X struct {
		Enabled        bool     `json:"enabled"`
		MaxPerPoll     int      `json:"max_posts_per_poll"`
		MaxPerDay      int      `json:"max_posts_per_day"`
		MinInterval    Duration `json:"min_interval"`
		ExcludeReplies bool     `json:"exclude_replies"`
		HomeTimeline   bool     `json:"home_timeline"`
		Mentions       bool     `json:"mentions"`
		Accounts       []string `json:"accounts"`
	} `json:"x"`
	// XSearch reads X accounts through twitterapi.io (unofficial, billed per
	// tweet returned) instead of the official X API.
	XSearch struct {
		Enabled        bool     `json:"enabled"`
		Accounts       []string `json:"accounts"`
		AlwaysDigest   []string `json:"always_digest"` // subset of accounts, like x.accounts
		ExcludeReplies bool     `json:"exclude_replies"`
		IncludeReposts bool     `json:"include_reposts"`
		MinInterval    Duration `json:"min_interval"`
		MaxPerDay      int      `json:"max_posts_per_day"`
	} `json:"xsearch"`
	LLM struct {
		ClaudePath   string `json:"claude_path"`
		HomeDir      string `json:"home_dir"`
		Model        string `json:"model"`
		ScoreEffort  string `json:"score_effort"`
		DigestEffort string `json:"digest_effort"`
		Interests    string `json:"interests"`
	} `json:"llm"`
	Digest struct {
		// EachItemOnce: every item appears in one message only. Off, the
		// daily digests also repeat what hourly updates showed.
		EachItemOnce bool `json:"each_item_once"`
	} `json:"digest"`
	Thresholds struct {
		Urgent        int `json:"urgent"`
		Digest        int `json:"digest"`
		MaxUrgentPoll int `json:"max_urgent_per_poll"`
	} `json:"thresholds"`
	Telegram struct {
		ChatID string `json:"chat_id"`
	} `json:"telegram"`

	Secrets Secrets `json:"-"`
}

// Secrets come from the environment only, never from the config file.
type Secrets struct {
	ClaudeOAuthToken string
	TelegramToken    string
	XConsumerKey     string
	XConsumerSecret  string
	XAccessToken     string
	XAccessSecret    string
	TwitterAPIKey    string // twitterapi.io, for xsearch
}

var xUsername = regexp.MustCompile(`^[A-Za-z0-9_]{1,15}$`)

// chatID is a Telegram chat id: a number (negative for groups) or a public
// channel's @username.
var chatID = regexp.MustCompile(`^(-?[0-9]{1,20}|@[A-Za-z0-9_]{5,32})$`)

var efforts = map[string]bool{"low": true, "medium": true, "high": true, "xhigh": true, "max": true}

// Load reads path, applies defaults, reads secrets and validates everything.
func Load(path string, getenv func(string) string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	c := &Config{}
	c.DBPath = "/var/lib/newsagent/newsagent.db"
	c.MaxAge = Duration(48 * time.Hour)
	c.Timezone = "UTC"
	c.HN.TopN = 30
	c.X.MaxPerPoll = 25
	c.X.MaxPerDay = 200
	c.X.MinInterval = Duration(time.Hour)
	c.X.HomeTimeline = true
	c.XSearch.MinInterval = Duration(15 * time.Minute)
	c.XSearch.MaxPerDay = 2000
	c.XSearch.IncludeReposts = true
	c.LLM.ClaudePath = "claude"
	c.LLM.HomeDir = "/var/lib/newsagent"
	c.LLM.Model = "claude-opus-5-5"
	c.LLM.ScoreEffort = "low"
	c.LLM.DigestEffort = "medium"
	c.Digest.EachItemOnce = true
	c.Thresholds.Urgent = 9
	c.Thresholds.Digest = 6
	c.Thresholds.MaxUrgentPoll = 3

	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	c.Secrets = Secrets{
		ClaudeOAuthToken: getenv("CLAUDE_CODE_OAUTH_TOKEN"),
		TelegramToken:    getenv("TELEGRAM_BOT_TOKEN"),
		XConsumerKey:     getenv("X_CONSUMER_KEY"),
		XConsumerSecret:  getenv("X_CONSUMER_SECRET"),
		XAccessToken:     getenv("X_ACCESS_TOKEN"),
		XAccessSecret:    getenv("X_ACCESS_TOKEN_SECRET"),
		TwitterAPIKey:    getenv("TWITTERAPI_IO_KEY"),
	}
	return c, c.validate()
}

func (c *Config) validate() error {
	var errs []error
	check := func(ok bool, msg string) {
		if !ok {
			errs = append(errs, errors.New(msg))
		}
	}
	check(c.DBPath != "", "db_path is empty")
	check(c.MaxAge > 0 && time.Duration(c.MaxAge) < store.Retention,
		fmt.Sprintf("max_age must be > 0 and < %s (items are pruned after that)", store.Retention))
	if _, err := time.LoadLocation(c.Timezone); err != nil {
		errs = append(errs, fmt.Errorf("timezone: %w", err))
	}
	if c.HN.Enabled {
		check(c.HN.TopN > 0 && c.HN.TopN <= 500, "hn.top_n must be 1..500")
	}
	names := map[string]bool{"hn": true, "x": true}
	for _, f := range c.Feeds {
		check(f.Name != "" && !names[f.Name], fmt.Sprintf("feed name %q is empty or not unique", f.Name))
		check(strings.HasPrefix(f.URL, "https://") || strings.HasPrefix(f.URL, "http://"), fmt.Sprintf("feed %q: url must be http(s)", f.Name))
		names[f.Name] = true
	}
	if c.X.Enabled {
		check(c.X.MaxPerPoll >= 5 && c.X.MaxPerPoll <= 100, "x.max_posts_per_poll must be 5..100")
		check(c.X.MaxPerDay >= c.X.MaxPerPoll, "x.max_posts_per_day must be >= x.max_posts_per_poll")
		check(c.X.MinInterval > 0, "x.min_interval must be > 0")
		check(c.X.HomeTimeline || c.X.Mentions || len(c.X.Accounts) > 0, "x is enabled but home_timeline, mentions and accounts are all off")
		check(len(c.X.Accounts) <= 100, "x.accounts: at most 100")
		seen := map[string]bool{}
		for _, a := range c.X.Accounts {
			check(xUsername.MatchString(a), fmt.Sprintf("x.accounts: %q is not a valid username (no @)", a))
			check(!seen[strings.ToLower(a)], fmt.Sprintf("x.accounts: %q is listed twice", a))
			seen[strings.ToLower(a)] = true
		}
		s := c.Secrets
		check(s.XConsumerKey != "" && s.XConsumerSecret != "" && s.XAccessToken != "" && s.XAccessSecret != "",
			"x is enabled but X_CONSUMER_KEY, X_CONSUMER_SECRET, X_ACCESS_TOKEN or X_ACCESS_TOKEN_SECRET is missing")
	}
	if c.XSearch.Enabled {
		check(c.Secrets.TwitterAPIKey != "", "xsearch is enabled but TWITTERAPI_IO_KEY is missing")
		check(len(c.XSearch.Accounts) >= 1 && len(c.XSearch.Accounts) <= 100, "xsearch.accounts: 1..100 usernames")
		seen := map[string]bool{}
		for _, a := range c.XSearch.Accounts {
			check(xUsername.MatchString(a), fmt.Sprintf("xsearch.accounts: %q is not a valid username (no @)", a))
			check(!seen[strings.ToLower(a)], fmt.Sprintf("xsearch.accounts: %q is listed twice", a))
			seen[strings.ToLower(a)] = true
		}
		for _, a := range c.XSearch.AlwaysDigest {
			check(seen[strings.ToLower(a)], fmt.Sprintf("xsearch.always_digest: %q is not in xsearch.accounts", a))
		}
		if c.X.Enabled {
			for _, a := range c.X.Accounts {
				// Read through both APIs, it would be billed twice per poll.
				check(!seen[strings.ToLower(a)], fmt.Sprintf("%q is in both x.accounts and xsearch.accounts", a))
			}
		}
		check(c.XSearch.MinInterval > 0, "xsearch.min_interval must be > 0")
		check(c.XSearch.MaxPerDay >= 20, "xsearch.max_posts_per_day must be >= 20")
	}
	check(c.LLM.Model != "" && c.LLM.ClaudePath != "", "llm.model or llm.claude_path is empty")
	check(strings.HasPrefix(c.LLM.HomeDir, "/"), "llm.home_dir must be an absolute path")
	check(efforts[c.LLM.ScoreEffort] && efforts[c.LLM.DigestEffort], "llm efforts must be low|medium|high|xhigh|max")
	check(strings.TrimSpace(c.LLM.Interests) != "", "llm.interests is empty")
	check(c.Thresholds.Urgent >= 0 && c.Thresholds.Urgent <= 10 && c.Thresholds.Digest >= 0 && c.Thresholds.Digest <= 10,
		"thresholds must be 0..10")
	check(c.Thresholds.MaxUrgentPoll >= 0, "thresholds.max_urgent_per_poll must be >= 0")
	check(chatID.MatchString(c.Telegram.ChatID), fmt.Sprintf("telegram.chat_id %q must be a number or @channelname", c.Telegram.ChatID))
	check(c.Secrets.ClaudeOAuthToken != "", "CLAUDE_CODE_OAUTH_TOKEN is missing (run: claude setup-token)")
	check(c.Secrets.TelegramToken != "", "TELEGRAM_BOT_TOKEN is missing")
	return errors.Join(errs...)
}
