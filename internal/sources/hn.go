package sources

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/al-maisan/newsagent/internal/store"
)

// HNBaseURL is the official Hacker News API.
const HNBaseURL = "https://hacker-news.firebaseio.com/v0"

// HN fetches the current top stories from Hacker News.
type HN struct {
	Client  *http.Client
	BaseURL string
	TopN    int
}

type hnItem struct {
	ID          int64  `json:"id"`
	Type        string `json:"type"`
	By          string `json:"by"`
	Time        int64  `json:"time"`
	Title       string `json:"title"`
	URL         string `json:"url"`
	Score       int    `json:"score"`
	Descendants int    `json:"descendants"`
	Dead        bool   `json:"dead"`
	Deleted     bool   `json:"deleted"`
}

// Fetch returns the top N stories. Single item failures are skipped.
func (h *HN) Fetch(ctx context.Context) ([]store.Item, error) {
	var ids []int64
	if err := getJSON(ctx, h.Client, h.BaseURL+"/topstories.json", nil, &ids); err != nil {
		return nil, fmt.Errorf("hn top stories: %w", err)
	}
	if len(ids) > h.TopN {
		ids = ids[:h.TopN]
	}
	got := make([]*hnItem, len(ids))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for i, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			var it hnItem
			if err := getJSON(ctx, h.Client, fmt.Sprintf("%s/item/%d.json", h.BaseURL, id), nil, &it); err == nil {
				got[i] = &it
			}
		}()
	}
	wg.Wait()

	var out []store.Item
	for _, it := range got {
		if it == nil || it.Type != "story" || it.Dead || it.Deleted || it.Title == "" {
			continue
		}
		discuss := "https://news.ycombinator.com/item?id=" + strconv.FormatInt(it.ID, 10)
		u := it.URL
		if !isWebURL(u) {
			u = discuss
		}
		out = append(out, store.Item{
			Source:     "hn",
			ExtID:      strconv.FormatInt(it.ID, 10),
			Title:      clip(plain(it.Title), 300),
			URL:        u,
			DiscussURL: discuss,
			Author:     it.By,
			Text:       fmt.Sprintf("%d points, %d comments", it.Score, it.Descendants),
			Published:  time.Unix(it.Time, 0).UTC(),
		})
	}
	if len(out) == 0 && len(ids) > 0 {
		return nil, fmt.Errorf("hn: all %d item fetches failed", len(ids))
	}
	return out, nil
}
