package app

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/al-maisan/newsagent/internal/llm"
	"github.com/al-maisan/newsagent/internal/store"
	"github.com/al-maisan/newsagent/internal/telegram"
)

type fakeModel struct {
	scores     map[string]int  // title -> score
	skip       map[string]bool // titles Score leaves out
	scoreErr   error
	sections   []llm.Section
	scoreCall  int
	reported   [][]string // alreadyReported per Score call
	skipped    []int64    // Digest's skipped_ids
	sentTitles [][]string // alreadySent per Digest call
}

func (f *fakeModel) Score(_ context.Context, items []store.Item, reported []string) ([]store.Score, error) {
	f.scoreCall++
	f.reported = append(f.reported, reported)
	if f.scoreErr != nil {
		return nil, f.scoreErr
	}
	var out []store.Score
	for _, it := range items {
		if !f.skip[it.Title] {
			out = append(out, store.Score{ID: it.ID, Score: f.scores[it.Title], Summary: "sum " + it.Title})
		}
	}
	return out, nil
}

func (f *fakeModel) Digest(_ context.Context, items []store.Item, sent []string) ([]llm.Section, []int64, error) {
	f.sentTitles = append(f.sentTitles, sent)
	if f.sections != nil {
		return f.sections, f.skipped, nil
	}
	var ids []int64
	for _, it := range items {
		ids = append(ids, it.ID)
	}
	return []llm.Section{{Heading: "Tech", Entries: []llm.Entry{{Text: "Things <happened>", ItemIDs: append(ids, 9999)}}}}, f.skipped, nil
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
	if err := a.Digest(context.Background(), store.Daily); err != nil {
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
	if err := a.Digest(context.Background(), store.Daily); err != nil {
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
	if err := a.Digest(context.Background(), store.Daily); err == nil {
		t.Fatal("want error when nothing can be rendered")
	}
	if len(*sent) != 0 {
		t.Fatalf("model-invented entry sent: %q", *sent)
	}
	// The item was not marked: a later digest still carries it.
	m.sections = nil
	if err := a.Digest(context.Background(), store.Daily); err != nil {
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
	if err := a.Digest(context.Background(), store.Daily); err != nil {
		t.Fatal(err)
	}
	if len(*sent) != 1 || strings.Contains((*sent)[0], "https://e.com/2") {
		t.Fatalf("bad first digest: %q", *sent)
	}
	m.sections = nil
	if err := a.Digest(context.Background(), store.Daily); err != nil {
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
	if err := a.Digest(context.Background(), store.Daily); err != nil {
		t.Fatal(err)
	}
	msg := (*sent)[len(*sent)-1]
	if !strings.Contains(msg, "From accounts you follow") || !strings.Contains(msg, "@esr: low") {
		t.Fatalf("followed account dropped: %q", msg)
	}
}

func TestPollLaterBatchSeesPendingUrgent(t *testing.T) {
	m := &fakeModel{scores: map[string]int{"Huge": 10}}
	items := []store.Item{item("0", "Huge")}
	for i := 1; i <= scoreBatch; i++ {
		items = append(items, item(fmt.Sprint(i), fmt.Sprint("minor ", i)))
	}
	a, _ := newApp(t, m, items)
	if err := a.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m.scoreCall != 2 || len(m.reported[0]) != 0 || len(m.reported[1]) != 1 || m.reported[1][0] != "Huge" {
		t.Fatalf("second batch must see the pending ping: calls=%d reported=%q", m.scoreCall, m.reported)
	}
}

func TestHourlyThenDaily(t *testing.T) {
	m := &fakeModel{scores: map[string]int{"A": 7}}
	a, sent := newApp(t, m, []store.Item{item("1", "A")})
	a.Poll(context.Background())
	if err := a.Digest(context.Background(), store.Hourly); err != nil {
		t.Fatal(err)
	}
	if len(*sent) != 1 || !strings.Contains((*sent)[0], "Hourly update") {
		t.Fatalf("want hourly update, got %q", *sent)
	}
	// Next hour: nothing new, nothing sent.
	a.Digest(context.Background(), store.Hourly)
	if len(*sent) != 1 {
		t.Fatalf("hourly repeated items: %q", *sent)
	}
	// The daily doesn't repeat what an hourly update showed.
	if err := a.Digest(context.Background(), store.Daily); err != nil {
		t.Fatal(err)
	}
	if len(*sent) != 1 {
		t.Fatalf("daily repeated an hourly item: %q", *sent)
	}
	// A new item goes into the next digest of either kind, once.
	a.Store.Insert(context.Background(), []store.Item{item("2", "A")}, now)
	m.scores["A"] = 7
	a.Poll(context.Background())
	if err := a.Digest(context.Background(), store.Daily); err != nil {
		t.Fatal(err)
	}
	if len(*sent) != 2 || !strings.Contains((*sent)[1], "News digest") || !strings.Contains((*sent)[1], "https://e.com/2") {
		t.Fatalf("daily must carry the new item: %q", *sent)
	}
	a.Digest(context.Background(), store.Hourly)
	if len(*sent) != 2 {
		t.Fatalf("hourly repeated a daily item: %q", *sent)
	}
}

func TestPollTransientFailureGivesUpLater(t *testing.T) {
	m := &fakeModel{scoreErr: &llm.TransientError{Err: errors.New("usage limit")}}
	a, _ := newApp(t, m, []store.Item{item("1", "X")})
	for range store.MaxScoreRetries + 2 {
		a.Poll(context.Background())
	}
	if m.scoreCall != store.MaxScoreRetries {
		t.Fatalf("want %d tries, got %d", store.MaxScoreRetries, m.scoreCall)
	}
}

func TestPollRejectedUrgentSkipped(t *testing.T) {
	m := &fakeModel{scores: map[string]int{"Huge": 10, "Big": 9}}
	a, sent := newApp(t, m, []store.Item{item("1", "Huge"), item("2", "Big")})
	send := a.Send
	tries := 0
	a.Send = func(ctx context.Context, s string) error {
		if strings.Contains(s, "Huge") {
			tries++
			return &telegram.APIError{Code: 400, Description: "Bad Request: message is too long"}
		}
		return send(ctx, s)
	}
	if err := a.Poll(context.Background()); err == nil {
		t.Fatal("want error for the rejected ping")
	}
	if len(*sent) != 1 || !strings.Contains((*sent)[0], "Big") {
		t.Fatalf("ping behind a rejected one not sent: %q", *sent)
	}
	if err := a.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if tries != 1 || len(*sent) != 1 {
		t.Fatalf("rejected ping retried: tries=%d sent=%q", tries, *sent)
	}
}

func TestDigestRejectedPartSkipped(t *testing.T) {
	m := &fakeModel{scores: map[string]int{"A": 7}}
	a, sent := newApp(t, m, []store.Item{item("1", "A")})
	a.Poll(context.Background())
	send := a.Send
	a.Send = func(context.Context, string) error {
		return &telegram.APIError{Code: 400, Description: "Bad Request: can't parse entities"}
	}
	if err := a.Digest(context.Background(), store.Daily); err == nil {
		t.Fatal("want error for the rejected part")
	}
	a.Send = send
	if err := a.Digest(context.Background(), store.Daily); err != nil {
		t.Fatal(err)
	}
	// The item never reached the reader, so the next digest carries it.
	if len(*sent) != 1 || !strings.Contains((*sent)[0], "https://e.com/1") {
		t.Fatalf("rejected item not retried: %q", *sent)
	}
}

func TestDigestMarksOnlyDeliveredParts(t *testing.T) {
	var entries []llm.Entry
	var items []store.Item
	scores := map[string]int{}
	for i := 1; i <= 8; i++ {
		title := fmt.Sprint("T", i)
		scores[title] = 7
		items = append(items, item(fmt.Sprint(i), title))
		entries = append(entries, llm.Entry{Text: strings.Repeat("x", 590), ItemIDs: []int64{int64(i)}})
	}
	m := &fakeModel{scores: scores, sections: []llm.Section{{Heading: "Long", Entries: entries}}}
	a, sent := newApp(t, m, items)
	a.Poll(context.Background())
	send := a.Send
	var parts int
	a.Send = func(ctx context.Context, s string) error {
		parts++
		if strings.Contains(s, `href="https://e.com/1"`) {
			return &telegram.APIError{Code: 400, Description: "Bad Request: message is too long"}
		}
		return send(ctx, s)
	}
	if err := a.Digest(context.Background(), store.Daily); err == nil || parts < 2 {
		t.Fatalf("want a rejected part among %d parts, err=%v", parts, err)
	}
	a.Send = send
	m.sections = nil
	*sent = nil
	if err := a.Digest(context.Background(), store.Daily); err != nil {
		t.Fatal(err)
	}
	// Only the rejected part's items come back; delivered ones don't.
	if len(*sent) != 1 || !strings.Contains((*sent)[0], `href="https://e.com/1"`) || strings.Contains((*sent)[0], `href="https://e.com/8"`) {
		t.Fatalf("bad retry digest: %q", *sent)
	}
}

func TestPollStopsAfterSkippedItems(t *testing.T) {
	m := &fakeModel{skip: map[string]bool{"skipped": true}}
	items := []store.Item{item("0", "skipped")}
	for i := 1; i <= scoreBatch; i++ {
		items = append(items, item(fmt.Sprint(i), fmt.Sprint("minor ", i)))
	}
	a, _ := newApp(t, m, items)
	for poll := 1; poll <= store.MaxScoreAttempts; poll++ {
		a.Poll(context.Background())
		// One score call per poll: a batch with a skipped item ends the loop,
		// so the skipped item is not retried within the same poll.
		if m.scoreCall != poll {
			t.Fatalf("poll %d: want %d score calls in total, got %d", poll, poll, m.scoreCall)
		}
	}
}

func TestUsageLimitAlarm(t *testing.T) {
	limit := &llm.TransientError{Err: errors.New("claude cli: You've hit your 5-hour limit · resets 7:30am (UTC)"), UsageLimit: true}
	m := &fakeModel{scoreErr: limit, scores: map[string]int{}}
	a, sent := newApp(t, m, []store.Item{item("1", "A")})
	clock := now
	a.Now = func() time.Time { return clock }
	alarms := func() int {
		n := 0
		for _, s := range *sent {
			if strings.Contains(s, "Claude usage limit hit") {
				n++
			}
		}
		return n
	}
	a.Poll(context.Background())
	if alarms() != 1 || !strings.Contains((*sent)[0], "resets 7:30am (UTC)") {
		t.Fatalf("want one alarm with the reset time, got %q", *sent)
	}
	clock = clock.Add(15 * time.Minute)
	a.Poll(context.Background())
	if alarms() != 1 {
		t.Fatalf("alarm repeated within %s: %q", limitRepeat, *sent)
	}
	clock = clock.Add(limitRepeat)
	a.Poll(context.Background())
	if alarms() != 2 {
		t.Fatalf("want a reminder after %s, got %d alarms", limitRepeat, alarms())
	}
	// A working call ends the episode; the next hit alarms at once.
	m.scoreErr = nil
	clock = clock.Add(15 * time.Minute)
	a.Store.Insert(context.Background(), []store.Item{item("2", "B")}, clock)
	a.Poll(context.Background())
	m.scoreErr = limit
	a.Store.Insert(context.Background(), []store.Item{item("3", "C")}, clock)
	clock = clock.Add(15 * time.Minute)
	a.Poll(context.Background())
	if alarms() != 3 {
		t.Fatalf("want an alarm right after recovery, got %d", alarms())
	}
}

func TestRaiseOncePerEpisode(t *testing.T) {
	a, sent := newApp(t, &fakeModel{}, nil)
	ctx := context.Background()
	a.Raise(ctx, "k", "T", "d <b>", 0)
	a.Raise(ctx, "k", "T", "d <b>", 0)
	if len(*sent) != 1 || !strings.Contains((*sent)[0], "d &lt;b&gt;") {
		t.Fatalf("want one escaped alarm, got %q", *sent)
	}
	a.Clear(ctx, "k")
	a.Raise(ctx, "k", "T", "d", 0)
	if len(*sent) != 2 {
		t.Fatalf("want an alarm after Clear, got %q", *sent)
	}
}

func TestPollFutureDatedItemIsUndated(t *testing.T) {
	future := item("1", "From the future")
	future.Published = now.Add(30 * 24 * time.Hour)
	soon := item("2", "Slightly ahead")
	soon.Published = now.Add(time.Hour) // clock skew: keep the date
	a, _ := newApp(t, &fakeModel{}, []store.Item{future, soon})
	if err := a.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Undated items survive the 30-day prune; dated ones don't.
	if n, err := a.Store.Prune(context.Background(), now.Add(store.Retention+time.Hour)); err != nil || n != 1 {
		t.Fatalf("want only the slightly-ahead item pruned, got %d %v", n, err)
	}
	if n, _ := a.Store.Insert(context.Background(), []store.Item{future}, now.Add(store.Retention+time.Hour)); n != 0 {
		t.Fatal("future-dated item came back as new after pruning")
	}
}

func TestUsageLimitKeepsItems(t *testing.T) {
	limit := &llm.TransientError{Err: errors.New("claude cli: You've hit your weekly limit · resets Mon"), UsageLimit: true}
	m := &fakeModel{scoreErr: limit, scores: map[string]int{"A": 7}}
	a, _ := newApp(t, m, []store.Item{item("1", "A")})
	for range store.MaxScoreRetries + 2 {
		a.Poll(context.Background())
	}
	if m.scoreCall != store.MaxScoreRetries+2 {
		t.Fatalf("usage limit must not use up retries: %d calls", m.scoreCall)
	}
	m.scoreErr = nil
	a.Poll(context.Background())
	if un, _ := a.Store.Unscored(context.Background(), now.Add(-time.Hour), 10); len(un) != 0 {
		t.Fatalf("item not scored after the limit reset: %+v", un)
	}
}

func TestDigestSkipsRepeats(t *testing.T) {
	m := &fakeModel{scores: map[string]int{"A": 7, "B": 7}}
	a, sent := newApp(t, m, []store.Item{item("1", "A")})
	a.Poll(context.Background())
	if err := a.Digest(context.Background(), store.Daily); err != nil {
		t.Fatal(err)
	}
	// A new item about the same story: the hourly model is told what went
	// out and skips it as a repeat.
	a.Store.Insert(context.Background(), []store.Item{item("2", "B")}, now)
	a.Poll(context.Background())
	m.sections = []llm.Section{{Heading: "X", Entries: []llm.Entry{{Text: "nothing", ItemIDs: []int64{12345}}}}}
	m.skipped = []int64{2, 777} // 777: not an item it was given, ignored
	if err := a.Digest(context.Background(), store.Hourly); err != nil {
		t.Fatalf("only repeats must not be an error: %v", err)
	}
	if len(*sent) != 1 {
		t.Fatalf("a digest of only repeats must send nothing: %q", *sent)
	}
	if got := m.sentTitles[1]; len(got) != 1 || got[0] != "A" {
		t.Fatalf("hourly model not told what went out: %q", got)
	}
	// The repeat is marked: it doesn't come back in the next hourly update.
	m.sections, m.skipped = nil, nil
	a.Digest(context.Background(), store.Hourly)
	if len(*sent) != 1 {
		t.Fatalf("skipped repeat came back: %q", *sent)
	}
}

func TestDailyRecapRepeatsHourly(t *testing.T) {
	m := &fakeModel{scores: map[string]int{"A": 7}}
	a, sent := newApp(t, m, []store.Item{item("1", "A")})
	a.DailyRecap = true
	a.Poll(context.Background())
	if err := a.Digest(context.Background(), store.Hourly); err != nil {
		t.Fatal(err)
	}
	if err := a.Digest(context.Background(), store.Daily); err != nil {
		t.Fatal(err)
	}
	if len(*sent) != 2 || !strings.Contains((*sent)[1], "News digest") || !strings.Contains((*sent)[1], "https://e.com/1") {
		t.Fatalf("recap daily must repeat the hourly item: %q", *sent)
	}
	// After the daily, the next daily doesn't repeat it again.
	a.Digest(context.Background(), store.Daily)
	if len(*sent) != 2 {
		t.Fatalf("daily repeated itself: %q", *sent)
	}
}
