package app

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/al-maisan/newsagent/internal/llm"
	"github.com/al-maisan/newsagent/internal/store"
)

type fakeModel struct {
	scores    map[string]int // title -> score
	scoreErr  error
	sections  []llm.Section
	scoreCall int
}

func (f *fakeModel) Score(_ context.Context, items []store.Item, _ []string) ([]store.Score, error) {
	f.scoreCall++
	if f.scoreErr != nil {
		return nil, f.scoreErr
	}
	var out []store.Score
	for _, it := range items {
		out = append(out, store.Score{ID: it.ID, Score: f.scores[it.Title], Summary: "sum " + it.Title})
	}
	return out, nil
}

func (f *fakeModel) Digest(_ context.Context, items []store.Item) ([]llm.Section, error) {
	if f.sections != nil {
		return f.sections, nil
	}
	var ids []int64
	for _, it := range items {
		ids = append(ids, it.ID)
	}
	return []llm.Section{{Heading: "Tech", Entries: []llm.Entry{{Text: "Things <happened>", ItemIDs: append(ids, 9999)}}}}, nil
}

var now = time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)

func newApp(t *testing.T, m *fakeModel, items []store.Item) (*App, *[]string) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	var sent []string
	return &App{
		Store:         st,
		Sources:       []Source{{Name: "feed", Fetch: func(context.Context) ([]store.Item, error) { return items, nil }}},
		Model:         m,
		Send:          func(_ context.Context, s string) error { sent = append(sent, s); return nil },
		Now:           func() time.Time { return now },
		Loc:           time.UTC,
		MaxAge:        48 * time.Hour,
		UrgentScore:   9,
		DigestScore:   6,
		MaxUrgentPoll: 3,
		Labels:        map[string]string{},
	}, &sent
}

func item(id, title string) store.Item {
	return store.Item{Source: "feed", ExtID: id, Title: title, URL: "https://e.com/" + id, Published: now.Add(-time.Hour)}
}

func TestPollUrgentOnce(t *testing.T) {
	m := &fakeModel{scores: map[string]int{"Huge": 10, "Meh": 3}}
	old := item("old", "Old")
	old.Published = now.Add(-72 * time.Hour)
	a, sent := newApp(t, m, []store.Item{item("1", "Huge"), item("2", "Meh"), old})
	if err := a.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(*sent) != 1 || !strings.Contains((*sent)[0], "Huge") || !strings.Contains((*sent)[0], `href="https://e.com/1"`) {
		t.Fatalf("want one urgent ping, got %q", *sent)
	}
	// Second poll: same items are known, nothing re-sent or re-scored.
	if err := a.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(*sent) != 1 || m.scoreCall != 1 {
		t.Fatalf("re-sent or re-scored: sent=%d scoreCalls=%d", len(*sent), m.scoreCall)
	}
}

func TestPollSendFailureRetries(t *testing.T) {
	m := &fakeModel{scores: map[string]int{"Huge": 10}}
	a, sent := newApp(t, m, []store.Item{item("1", "Huge")})
	send := a.Send
	a.Send = func(context.Context, string) error { return errors.New("down") }
	if err := a.Poll(context.Background()); err == nil {
		t.Fatal("want error")
	}
	a.Send = send
	if err := a.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(*sent) != 1 {
		t.Fatalf("urgent not retried: %q", *sent)
	}
}

func TestPollScoreFailureGivesUp(t *testing.T) {
	m := &fakeModel{scoreErr: errors.New("bad request")}
	a, _ := newApp(t, m, []store.Item{item("1", "X")})
	for range store.MaxScoreAttempts + 2 {
		a.Poll(context.Background())
	}
	if m.scoreCall != store.MaxScoreAttempts {
		t.Fatalf("want %d attempts, got %d", store.MaxScoreAttempts, m.scoreCall)
	}
}

func TestDigest(t *testing.T) {
	m := &fakeModel{scores: map[string]int{"A": 7, "B": 6, "C": 2}}
	a, sent := newApp(t, m, []store.Item{item("1", "A"), item("2", "B"), item("3", "C")})
	if err := a.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := a.Digest(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(*sent) != 1 {
		t.Fatalf("want 1 digest message, got %q", *sent)
	}
	msg := (*sent)[0]
	if !strings.Contains(msg, "Things &lt;happened&gt;") || !strings.Contains(msg, "https://e.com/1") ||
		!strings.Contains(msg, "https://e.com/2") || strings.Contains(msg, "https://e.com/3") {
		t.Fatalf("bad digest %q", msg)
	}
	// Items are marked: next digest has nothing to send.
	if err := a.Digest(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(*sent) != 1 {
		t.Fatalf("digest repeated items")
	}
}

func TestDigestDropsUnknownIDs(t *testing.T) {
	m := &fakeModel{scores: map[string]int{"A": 7}, sections: []llm.Section{
		{Heading: "Fake", Entries: []llm.Entry{{Text: "made up", ItemIDs: []int64{12345}}}},
	}}
	a, sent := newApp(t, m, []store.Item{item("1", "A")})
	a.Poll(context.Background())
	if err := a.Digest(context.Background()); err == nil {
		t.Fatal("want error when nothing can be rendered")
	}
	if len(*sent) != 0 {
		t.Fatalf("model-invented entry sent: %q", *sent)
	}
	// The item was not marked: a later digest still carries it.
	m.sections = nil
	if err := a.Digest(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(*sent) != 1 || !strings.Contains((*sent)[0], "https://e.com/1") {
		t.Fatalf("item lost: %q", *sent)
	}
}

func TestDigestKeepsLeftOutItems(t *testing.T) {
	m := &fakeModel{scores: map[string]int{"A": 7, "B": 6}, sections: []llm.Section{
		{Heading: "Tech", Entries: []llm.Entry{{Text: "about A", ItemIDs: []int64{1}}, {Text: " ", ItemIDs: []int64{2}}}},
	}}
	a, sent := newApp(t, m, []store.Item{item("1", "A"), item("2", "B")})
	a.Poll(context.Background())
	if err := a.Digest(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(*sent) != 1 || strings.Contains((*sent)[0], "https://e.com/2") {
		t.Fatalf("bad first digest: %q", *sent)
	}
	m.sections = nil
	if err := a.Digest(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(*sent) != 2 || !strings.Contains((*sent)[1], "https://e.com/2") || strings.Contains((*sent)[1], "https://e.com/1") {
		t.Fatalf("left-out item not carried over: %q", *sent)
	}
}

func TestMentionAlwaysPinged(t *testing.T) {
	m := &fakeModel{scores: map[string]int{"@fan: hi": 1}}
	it := item("1", "@fan: hi")
	it.Priority = store.PriorityPing
	a, sent := newApp(t, m, []store.Item{it})
	if err := a.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(*sent) != 1 || !strings.HasPrefix((*sent)[0], "💬") {
		t.Fatalf("mention not pinged: %q", *sent)
	}
}

func TestDigestKeepsFollowedAccounts(t *testing.T) {
	m := &fakeModel{scores: map[string]int{"A": 7, "@esr: low": 2}, sections: []llm.Section{
		{Heading: "Tech", Entries: []llm.Entry{{Text: "about A", ItemIDs: []int64{1}}}},
	}}
	follow := item("2", "@esr: low")
	follow.Priority = store.PriorityDigest
	a, sent := newApp(t, m, []store.Item{item("1", "A"), follow})
	a.Poll(context.Background())
	if err := a.Digest(context.Background()); err != nil {
		t.Fatal(err)
	}
	msg := (*sent)[len(*sent)-1]
	if !strings.Contains(msg, "From accounts you follow") || !strings.Contains(msg, "@esr: low") {
		t.Fatalf("followed account dropped: %q", msg)
	}
}
