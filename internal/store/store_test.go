package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
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

	// Scored 9 but not sent yet: already counts as reported.
	if titles, _ := s.UrgentTitles(ctx, 9, now.Add(-time.Hour)); len(titles) != 1 || titles[0] != "one" {
		t.Fatalf("pending titles %v", titles)
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
	if titles, _ := s.UrgentTitles(ctx, 9, now.Add(-time.Hour)); len(titles) != 1 || titles[0] != "one" {
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

func TestPriority(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	since := now.Add(-time.Hour)
	ins := func(prio string) {
		if _, err := s.Insert(ctx, []Item{{Source: "x", ExtID: "1", Title: "p", URL: "https://x/1", Priority: prio}}, now); err != nil {
			t.Fatal(err)
		}
	}
	ins("")
	ins(PriorityPing)   // upgraded
	ins(PriorityDigest) // must not downgrade
	urg, _ := s.PendingUrgent(ctx, 9, since, 5)
	if len(urg) != 1 || urg[0].Priority != PriorityPing {
		t.Fatalf("unscored ping item must be pending: %+v", urg)
	}
	if dig, _ := s.PendingDigest(ctx, 6, since, 5); len(dig) != 0 {
		t.Fatalf("ping item must not be in digest: %+v", dig)
	}
	s.Insert(ctx, []Item{{Source: "x", ExtID: "2", Title: "d", URL: "https://x/2", Priority: PriorityDigest}}, now)
	if dig, _ := s.PendingDigest(ctx, 6, since, 5); len(dig) != 1 || dig[0].ExtID != "2" {
		t.Fatalf("unscored digest item must be in digest: %+v", dig)
	}
}

func TestMigrateAddsPriority(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	old := strings.Replace(schema, "priority       TEXT    NOT NULL DEFAULT '',\n", "", 1)
	if old == schema {
		t.Fatal("test setup: priority column not found in schema")
	}
	if _, err := db.Exec(old); err != nil {
		t.Fatal(err)
	}
	db.Exec(`INSERT INTO items (source, ext_id, title, url, fetched_at) VALUES ('a', '1', 't', 'https://e', 1)`)
	db.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Insert(context.Background(), []Item{{Source: "a", ExtID: "1", Title: "t", URL: "https://e", Priority: PriorityPing}}, time.Now()); err != nil {
		t.Fatalf("insert after migration: %v", err)
	}
}
