// Package config loads the JSON config file and secrets from the environment.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
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
	} `json:"x"`
	LLM struct {
		Model        string `json:"model"`
		ScoreEffort  string `json:"score_effort"`
		DigestEffort string `json:"digest_effort"`
		Interests    string `json:"interests"`
	} `json:"llm"`
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
	AnthropicKey    string
	TelegramToken   string
	XConsumerKey    string
	XConsumerSecret string
	XAccessToken    string
	XAccessSecret   string
}

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
	c.LLM.Model = "claude-opus-5-5"
	c.LLM.ScoreEffort = "low"
	c.LLM.DigestEffort = "medium"
	c.Thresholds.Urgent = 9
	c.Thresholds.Digest = 6
	c.Thresholds.MaxUrgentPoll = 3

	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	c.Secrets = Secrets{
		AnthropicKey:    getenv("ANTHROPIC_API_KEY"),
		TelegramToken:   getenv("TELEGRAM_BOT_TOKEN"),
		XConsumerKey:    getenv("X_CONSUMER_KEY"),
		XConsumerSecret: getenv("X_CONSUMER_SECRET"),
		XAccessToken:    getenv("X_ACCESS_TOKEN"),
		XAccessSecret:   getenv("X_ACCESS_TOKEN_SECRET"),
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
	check(c.MaxAge > 0, "max_age must be > 0")
	if _, err := time.LoadLocation(c.Timezone); err != nil {
		errs = append(errs, fmt.Errorf("timezone: %w", err))
	}
	check(c.HN.TopN > 0 && c.HN.TopN <= 500, "hn.top_n must be 1..500")
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
		s := c.Secrets
		check(s.XConsumerKey != "" && s.XConsumerSecret != "" && s.XAccessToken != "" && s.XAccessSecret != "",
			"x is enabled but X_CONSUMER_KEY, X_CONSUMER_SECRET, X_ACCESS_TOKEN or X_ACCESS_TOKEN_SECRET is missing")
	}
	check(c.LLM.Model != "", "llm.model is empty")
	check(efforts[c.LLM.ScoreEffort] && efforts[c.LLM.DigestEffort], "llm efforts must be low|medium|high|xhigh|max")
	check(strings.TrimSpace(c.LLM.Interests) != "", "llm.interests is empty")
	check(c.Thresholds.Urgent >= 0 && c.Thresholds.Urgent <= 10 && c.Thresholds.Digest >= 0 && c.Thresholds.Digest <= 10,
		"thresholds must be 0..10")
	check(c.Thresholds.MaxUrgentPoll >= 0, "thresholds.max_urgent_per_poll must be >= 0")
	check(c.Telegram.ChatID != "", "telegram.chat_id is empty")
	check(c.Secrets.AnthropicKey != "", "ANTHROPIC_API_KEY is missing")
	check(c.Secrets.TelegramToken != "", "TELEGRAM_BOT_TOKEN is missing")
	return errors.Join(errs...)
}
