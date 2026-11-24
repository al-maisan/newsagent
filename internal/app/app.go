// Package app runs the poll and digest jobs.
package app

import (
	"context"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"strings"
	"time"

	"github.com/al-maisan/newsagent/internal/llm"
	"github.com/al-maisan/newsagent/internal/store"
	"github.com/al-maisan/newsagent/internal/telegram"
)

// Source is one named item fetcher.
type Source struct {
	Name  string
	Fetch func(ctx context.Context) ([]store.Item, error)
}

// Model scores items and writes digests.
type Model interface {
	Score(ctx context.Context, items []store.Item, alreadyReported []string) ([]store.Score, error)
	Digest(ctx context.Context, items []store.Item) ([]llm.Section, error)
}

// App wires sources, storage, the model and Telegram together.
type App struct {
	Store         *store.Store
	Sources       []Source
	Model         Model
	Send          func(ctx context.Context, html string) error
	Now           func() time.Time
	Loc           *time.Location
	MaxAge        time.Duration
	UrgentScore   int
	DigestScore   int
	MaxUrgentPoll int
	Labels        map[string]string // source name -> display label
}

const (
	scoreBatch      = 50
	maxScoreBatches = 4
	scoreWindow     = 48 * time.Hour
	urgentWindow    = 24 * time.Hour
	digestWindow    = 36 * time.Hour
	digestMaxItems  = 80
	chunkLimit      = 4000 // below telegram.MaxLen to leave headroom
	maxEntryRunes   = 600
)

// Poll fetches all sources, scores new items and sends urgent pings.
func (a *App) Poll(ctx context.Context) error {
	now := a.Now()
	var errs []error
	failed := 0
	for _, src := range a.Sources {
		items, err := src.Fetch(ctx)
		if err != nil {
			failed++
			slog.Warn("fetch failed", "source", src.Name, "err", err)
			continue
		}
		fresh := items[:0]
		for _, it := range items {
			if it.Published.IsZero() || now.Sub(it.Published) <= a.MaxAge {
				fresh = append(fresh, it)
			}
		}
		n, err := a.Store.Insert(ctx, fresh, now)
		if err != nil {
			return fmt.Errorf("store %s: %w", src.Name, err)
		}
		slog.Info("fetched", "source", src.Name, "items", len(items), "new", n)
	}
	if failed > 0 && failed == len(a.Sources) {
		errs = append(errs, errors.New("all sources failed"))
	}
	if err := a.score(ctx, now); err != nil {
		errs = append(errs, err)
	}
	if err := a.sendUrgent(ctx, now); err != nil {
		errs = append(errs, err)
	}
	if n, err := a.Store.Prune(ctx, now); err != nil {
		errs = append(errs, fmt.Errorf("prune: %w", err))
	} else if n > 0 {
		slog.Info("pruned old items", "count", n)
	}
	return errors.Join(errs...)
}

func (a *App) score(ctx context.Context, now time.Time) error {
	for range maxScoreBatches {
		items, err := a.Store.Unscored(ctx, now.Add(-scoreWindow), scoreBatch)
		if err != nil || len(items) == 0 {
			return err
		}
		reported, err := a.Store.UrgentTitles(ctx, a.UrgentScore, now.Add(-urgentWindow))
		if err != nil {
			return err
		}
		ids := make([]int64, len(items))
		for i, it := range items {
			ids[i] = it.ID
		}
		scores, err := a.Model.Score(ctx, items, reported)
		if err != nil {
			if llm.IsTransient(err) {
				// Counted separately, with a higher cap, so a batch that
				// always fails "transiently" can't block newer items forever.
				if serr := a.Store.CountScoreRetry(ctx, ids); serr != nil {
					return errors.Join(err, serr)
				}
				return fmt.Errorf("score (will retry): %w", err)
			}
			// Count the attempt so a batch that always fails is given up.
			if serr := a.Store.SaveScores(ctx, ids, nil); serr != nil {
				return errors.Join(err, serr)
			}
			return fmt.Errorf("score: %w", err)
		}
		if err := a.Store.SaveScores(ctx, ids, scores); err != nil {
			return fmt.Errorf("save scores: %w", err)
		}
		slog.Info("scored", "items", len(items), "scored", len(scores))
		// Items the model skipped would come back first in the next batch;
		// leave them for the next poll instead of using up their attempts now.
		if len(items) < scoreBatch || len(scores) < len(items) {
			return nil
		}
	}
	return nil
}

func (a *App) sendUrgent(ctx context.Context, now time.Time) error {
	items, err := a.Store.PendingUrgent(ctx, a.UrgentScore, now.Add(-urgentWindow), a.MaxUrgentPoll)
	if err != nil || len(items) == 0 {
		return err
	}
	// A message Telegram rejects is marked anyway: resending it would fail
	// again and block every ping behind it.
	var errs []error
	for _, it := range items {
		if err := a.Send(ctx, a.renderUrgent(it)); err != nil {
			if !telegram.IsRejected(err) {
				return errors.Join(append(errs, fmt.Errorf("send urgent: %w", err))...)
			}
			slog.Error("urgent rejected, skipped", "id", it.ID, "source", it.Source, "err", err)
			errs = append(errs, fmt.Errorf("urgent %d rejected: %w", it.ID, err))
		} else {
			slog.Info("urgent sent", "id", it.ID, "source", it.Source, "score", it.Score)
		}
		if err := a.Store.MarkUrgentSent(ctx, it.ID, now); err != nil {
			return errors.Join(append(errs, fmt.Errorf("mark urgent: %w", err))...)
		}
	}
	return errors.Join(errs...)
}

// Digest sends a themed summary of the notable items since the last digest.
// Only items that appear in the message are marked, and only after every
// message part went out, so a failed send is retried by the next digest
// (parts already delivered may repeat). Items the model left out stay
// pending for the next digest. A part Telegram rejects is skipped: resending
// it would fail again.
func (a *App) Digest(ctx context.Context, kind store.DigestKind) error {
	now := a.Now()
	items, err := a.Store.PendingDigest(ctx, kind, a.DigestScore, now.Add(-digestWindow), digestMaxItems)
	if err != nil {
		return err
	}
	if len(items) == 0 {
		slog.Info("digest: nothing notable")
		return nil
	}
	sections, err := a.Model.Digest(ctx, items)
	if err != nil {
		return fmt.Errorf("digest: %w", err)
	}
	title := "📰 <b>News digest</b>"
	if kind == store.Hourly {
		title = "🕐 <b>Hourly update</b>"
	}
	msg, ids := a.renderDigest(title, sections, items, now)
	if len(ids) == 0 {
		return fmt.Errorf("digest: model output references none of the %d items", len(items))
	}
	var errs []error
	for i, part := range telegram.Split(msg, chunkLimit) {
		if err := a.Send(ctx, part); err != nil {
			if !telegram.IsRejected(err) {
				return errors.Join(append(errs, fmt.Errorf("send digest: %w", err))...)
			}
			slog.Error("digest part rejected, skipped", "part", i, "bytes", len(part), "err", err)
			errs = append(errs, fmt.Errorf("digest part %d rejected: %w", i, err))
		}
	}
	if err := a.Store.MarkDigestSent(ctx, kind, ids, now); err != nil {
		return errors.Join(append(errs, fmt.Errorf("mark digest: %w", err))...)
	}
	slog.Info("digest sent", "items", len(ids), "left_out", len(items)-len(ids), "sections", len(sections))
	return errors.Join(errs...)
}

func (a *App) renderUrgent(it store.Item) string {
	var b strings.Builder
	icon := "⚡"
	if it.Priority == store.PriorityPing {
		icon = "💬"
	}
	b.WriteString(icon + " <b>" + esc(it.Title) + "</b>\n")
	if it.Summary != "" {
		b.WriteString(esc(it.Summary) + "\n")
	}
	b.WriteString(a.links(it))
	return b.String()
}

// renderDigest returns the message and the ids of the items it shows.
func (a *App) renderDigest(title string, sections []llm.Section, items []store.Item, now time.Time) (string, []int64) {
	byID := make(map[int64]store.Item, len(items))
	for _, it := range items {
		byID[it.ID] = it
	}
	var b strings.Builder
	b.WriteString(title + " · " + esc(now.In(a.Loc).Format("Mon 2 Jan 15:04")) + "\n")
	used := map[int64]bool{}
	var ids []int64
	for _, s := range sections {
		var lines []string
		for _, e := range s.Entries {
			var refs []string
			seen := map[int64]bool{}
			for _, id := range e.ItemIDs {
				if it, ok := byID[id]; ok && !seen[id] {
					seen[id] = true
					refs = append(refs, a.links(it))
				}
			}
			text := strings.TrimSpace(e.Text)
			if len(refs) == 0 || text == "" {
				continue // drop entries that point at nothing we fetched
			}
			for _, id := range e.ItemIDs {
				if seen[id] && !used[id] {
					used[id] = true
					ids = append(ids, id)
				}
			}
			lines = append(lines, "• "+esc(clip(text, maxEntryRunes))+" "+strings.Join(refs, " "))
		}
		if len(lines) > 0 {
			b.WriteString("\n<b>" + esc(clip(strings.TrimSpace(s.Heading), 60)) + "</b>\n" + strings.Join(lines, "\n") + "\n")
		}
	}
	// Digest items must always appear, even if the model left them out.
	var missed []string
	for _, it := range items {
		if it.Priority == store.PriorityDigest && !used[it.ID] {
			missed = append(missed, "• "+esc(clip(it.Title, 200))+" "+a.links(it))
			ids = append(ids, it.ID)
		}
	}
	if len(missed) > 0 {
		b.WriteString("\n<b>From accounts you follow</b>\n" + strings.Join(missed, "\n") + "\n")
	}
	return b.String(), ids
}

// links renders the item's links; URLs always come from the store.
func (a *App) links(it store.Item) string {
	label := a.Labels[it.Source]
	if label == "" {
		label = it.Source
	}
	s := `<a href="` + esc(it.URL) + `">` + esc(label) + `</a>`
	if it.DiscussURL != "" && it.DiscussURL != it.URL {
		s += ` <a href="` + esc(it.DiscussURL) + `">💬</a>`
	}
	return s
}

func esc(s string) string { return html.EscapeString(s) }

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
