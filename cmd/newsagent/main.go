// Command newsagent watches tech news sources and your X timeline and keeps
// you up to date via Telegram.
//
// Usage: newsagent [-config path] poll|digest|hourly
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/al-maisan/newsagent/internal/app"
	"github.com/al-maisan/newsagent/internal/config"
	"github.com/al-maisan/newsagent/internal/llm"
	"github.com/al-maisan/newsagent/internal/sources"
	"github.com/al-maisan/newsagent/internal/store"
	"github.com/al-maisan/newsagent/internal/telegram"
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err := run(); err != nil {
		slog.Error("failed", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfgPath := flag.String("config", "/etc/newsagent/config.json", "config file")
	flag.Parse()
	cmd := flag.Arg(0)
	if flag.NArg() != 1 || (cmd != "poll" && cmd != "digest" && cmd != "hourly") {
		return fmt.Errorf("usage: newsagent [-config path] poll|digest|hourly")
	}
	cfg, err := config.Load(*cfgPath, os.Getenv)
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	ctx, cancel = context.WithTimeout(ctx, 14*time.Minute)
	defer cancel()

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()

	loc, _ := time.LoadLocation(cfg.Timezone) // validated in config.Load
	for _, f := range cfg.Feeds {
		if strings.HasPrefix(f.URL, "http://") {
			slog.Warn("feed uses plain http: its content can be changed in transit", "feed", f.Name)
		}
	}
	hc := &http.Client{Timeout: 30 * time.Second, Transport: sources.Transport(cfg.PrivateHosts()), CheckRedirect: sources.CheckRedirect}
	tg := &telegram.Client{HTTP: hc, BaseURL: telegram.BaseURL, Token: cfg.Secrets.TelegramToken, ChatID: cfg.Telegram.ChatID}
	a := &app.App{
		Store: st,
		Model: &llm.Claude{
			Path:         cfg.LLM.ClaudePath,
			HomeDir:      cfg.LLM.HomeDir,
			OAuthToken:   cfg.Secrets.ClaudeOAuthToken,
			Model:        cfg.LLM.Model,
			ScoreEffort:  cfg.LLM.ScoreEffort,
			DigestEffort: cfg.LLM.DigestEffort,
			Interests:    cfg.LLM.Interests,
		},
		Send:          tg.Send,
		Now:           time.Now,
		Loc:           loc,
		MaxAge:        time.Duration(cfg.MaxAge),
		UrgentScore:   cfg.Thresholds.Urgent,
		DigestScore:   cfg.Thresholds.Digest,
		MaxUrgentPoll: cfg.Thresholds.MaxUrgentPoll,
		Labels:        map[string]string{"hn": "HN", "x": "X", "reddit": "Reddit"},
	}
	if cfg.HN.Enabled {
		hn := &sources.HN{Client: hc, BaseURL: sources.HNBaseURL, TopN: cfg.HN.TopN}
		a.Sources = append(a.Sources, app.Source{Name: "hn", Fetch: hn.Fetch})
	}
	for _, f := range cfg.Feeds {
		feed := &sources.Feed{Client: hc, Name: f.Name, URL: f.URL}
		a.Sources = append(a.Sources, app.Source{Name: f.Name, Fetch: feed.Fetch})
	}
	if cfg.X.Enabled {
		s := cfg.Secrets
		x := &sources.X{
			Client:         hc,
			BaseURL:        sources.XBaseURL,
			Auth:           sources.OAuth1{ConsumerKey: s.XConsumerKey, ConsumerSecret: s.XConsumerSecret, Token: s.XAccessToken, TokenSecret: s.XAccessSecret},
			MaxPerPoll:     cfg.X.MaxPerPoll,
			MaxPerDay:      cfg.X.MaxPerDay,
			MinInterval:    time.Duration(cfg.X.MinInterval),
			ExcludeReplies: cfg.X.ExcludeReplies,
			HomeTimeline:   cfg.X.HomeTimeline,
			Mentions:       cfg.X.Mentions,
			Accounts:       cfg.X.Accounts,
			Alarms:         a,
		}
		a.Sources = append(a.Sources, app.Source{Name: "x", Fetch: func(ctx context.Context) ([]store.Item, error) {
			return x.Fetch(ctx, st, time.Now())
		}})
	}

	if cfg.Reddit.Enabled {
		rd := &sources.Reddit{
			Client:       hc,
			BaseURL:      sources.RedditBaseURL,
			TokenURL:     sources.RedditTokenURL,
			ClientID:     cfg.Secrets.RedditClientID,
			ClientSecret: cfg.Secrets.RedditClientSecret,
			Subreddits:   cfg.Reddit.Subreddits,
			TopN:         cfg.Reddit.TopN,
			Alarms:       a,
		}
		a.Sources = append(a.Sources, app.Source{Name: "reddit", Fetch: rd.Fetch})
	}

	switch cmd {
	case "poll":
		return a.Poll(ctx)
	case "hourly":
		return a.Digest(ctx, store.Hourly)
	}
	return a.Digest(ctx, store.Daily)
}
