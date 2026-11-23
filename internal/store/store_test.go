package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestStore(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	items := []Item{{Source: "a", ExtID: "1", Title: "one", URL: "https://e/1"}, {Source: "a", ExtID: "2", Title: "two", URL: "https://e/2"}}
	if n, err := s.Insert(ctx, items, now); err != nil || n != 2 {
		t.Fatalf("insert: n=%d err=%v", n, err)
	}
	if n, err := s.Insert(ctx, items, now); err != nil || n != 0 {
		t.Fatalf("re-insert must be ignored: n=%d err=%v", n, err)
	}
	un, err := s.Unscored(ctx, now.Add(-time.Hour), 10)
	if err != nil || len(un) != 2 {
		t.Fatalf("unscored: %v %v", un, err)
	}
	// Only item 1 gets a score; item 2 counts one failed attempt.
	if err := s.SaveScores(ctx, []int64{un[0].ID, un[1].ID}, []Score{{ID: un[0].ID, Score: 9, Summary: "s"}}); err != nil {
		t.Fatal(err)
	}
	if un, _ = s.Unscored(ctx, now.Add(-time.Hour), 10); len(un) != 1 || un[0].ExtID != "2" {
		t.Fatalf("want item 2 unscored, got %v", un)
	}
	for range MaxScoreAttempts {
		s.SaveScores(ctx, []int64{un[0].ID}, nil)
	}
	if un, _ = s.Unscored(ctx, now.Add(-time.Hour), 10); len(un) != 0 {
		t.Fatalf("item 2 should be given up, got %v", un)
	}

	urg, err := s.PendingUrgent(ctx, 9, now.Add(-time.Hour), 5)
	if err != nil || len(urg) != 1 || urg[0].Summary != "s" {
		t.Fatalf("urgent: %v %v", urg, err)
	}
	if err := s.MarkUrgentSent(ctx, urg[0].ID, now); err != nil {
		t.Fatal(err)
	}
	if urg, _ = s.PendingUrgent(ctx, 9, now.Add(-time.Hour), 5); len(urg) != 0 {
		t.Fatalf("urgent not marked")
	}
	if titles, _ := s.RecentUrgentTitles(ctx, now.Add(-time.Hour)); len(titles) != 1 || titles[0] != "one" {
		t.Fatalf("titles %v", titles)
	}

	if err := s.Set(ctx, "k", "v1"); err != nil {
		t.Fatal(err)
	}
	s.Set(ctx, "k", "v2")
	if v, _ := s.Get(ctx, "k"); v != "v2" {
		t.Fatalf("kv got %q", v)
	}
	if v, err := s.Get(ctx, "missing"); v != "" || err != nil {
		t.Fatalf("missing key: %q %v", v, err)
	}

	if n, err := s.Prune(ctx, now.Add(time.Second)); err != nil || n != 2 {
		t.Fatalf("prune: %d %v", n, err)
	}
}
