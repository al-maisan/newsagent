// Package store persists news items and small key/value state in SQLite.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Item is one news entry from any source.
type Item struct {
	ID         int64
	Source     string // e.g. "hn", "lobsters", "x"
	ExtID      string // source-specific unique id
	Title      string
	URL        string
	DiscussURL string
	Author     string
	Text       string // short plain-text context for the model
	Published  time.Time
	Score      int
	Summary    string
	Priority   string // "", PriorityDigest or PriorityPing
}

// Item priorities: digest items always reach the digest, ping items are
// always sent right away.
const (
	PriorityDigest = "digest"
	PriorityPing   = "ping"
)

// rank orders priorities so an item never loses one it already has.
const rank = `CASE %s WHEN 'ping' THEN 2 WHEN 'digest' THEN 1 ELSE 0 END`

// Score is the model's rating of one item.
type Score struct {
	ID        int64
	Score     int
	Summary   string
	Duplicate bool
}

const schema = `
CREATE TABLE IF NOT EXISTS items (
	id             INTEGER PRIMARY KEY,
	source         TEXT    NOT NULL,
	ext_id         TEXT    NOT NULL,
	title          TEXT    NOT NULL,
	url            TEXT    NOT NULL,
	discuss_url    TEXT    NOT NULL DEFAULT '',
	author         TEXT    NOT NULL DEFAULT '',
	text           TEXT    NOT NULL DEFAULT '',
	published_at   INTEGER NOT NULL DEFAULT 0,
	fetched_at     INTEGER NOT NULL,
	score          INTEGER,
	summary        TEXT    NOT NULL DEFAULT '',
	duplicate      INTEGER NOT NULL DEFAULT 0,
	score_attempts INTEGER NOT NULL DEFAULT 0,
	score_retries  INTEGER NOT NULL DEFAULT 0,
	urgent_sent_at INTEGER,
	digest_sent_at INTEGER,
	priority       TEXT    NOT NULL DEFAULT '',
	UNIQUE (source, ext_id)
);
CREATE INDEX IF NOT EXISTS items_fetched ON items (fetched_at);
CREATE TABLE IF NOT EXISTS kv (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);`

// MaxScoreAttempts is how often scoring is retried before an item is given up.
const MaxScoreAttempts = 3

// MaxScoreRetries is how often scoring may fail transiently (usage limit,
// CLI crash) before an item is given up: about 6 hours of 15-minute polls.
// It keeps one batch that always fails from blocking all newer items.
const MaxScoreRetries = 24

// Store wraps the SQLite database.
type Store struct{ db *sql.DB }

// Open opens (and if needed creates) the database at path.
func Open(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db}, nil
}

// migrate adds columns that databases created by older versions lack.
// The check and the ALTERs run in one write transaction (BEGIN IMMEDIATE),
// so units that start at the same time upgrade the database only once.
func migrate(db *sql.DB) error {
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return err
	}
	for _, c := range []struct{ name, def string }{
		{"priority", "TEXT NOT NULL DEFAULT ''"},
		{"score_retries", "INTEGER NOT NULL DEFAULT 0"},
	} {
		var n int
		err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('items') WHERE name = ?`, c.name).Scan(&n)
		if err == nil && n == 0 {
			_, err = conn.ExecContext(ctx, `ALTER TABLE items ADD COLUMN `+c.name+` `+c.def)
		}
		if err != nil {
			conn.ExecContext(ctx, `ROLLBACK`)
			return err
		}
	}
	_, err = conn.ExecContext(ctx, `COMMIT`)
	return err
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// Insert stores items that are not known yet and returns how many were new.
// A known item gets the higher of its old and new priority, so a post seen
// in the home timeline first still counts once it shows up as a mention.
func (s *Store) Insert(ctx context.Context, items []Item, now time.Time) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `INSERT OR IGNORE INTO items
		(source, ext_id, title, url, discuss_url, author, text, published_at, fetched_at, priority)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()
	upd, err := tx.PrepareContext(ctx, `UPDATE items SET priority = ?
		WHERE source = ? AND ext_id = ? AND `+fmt.Sprintf(rank, "priority")+` < `+fmt.Sprintf(rank, "?"))
	if err != nil {
		return 0, err
	}
	defer upd.Close()
	n := 0
	for _, it := range items {
		res, err := stmt.ExecContext(ctx, it.Source, it.ExtID, it.Title, it.URL, it.DiscussURL,
			it.Author, it.Text, unix(it.Published), now.Unix(), it.Priority)
		if err != nil {
			return 0, fmt.Errorf("insert %s/%s: %w", it.Source, it.ExtID, err)
		}
		c, err := res.RowsAffected()
		if err != nil {
			return 0, err
		}
		n += int(c)
		if c == 0 && it.Priority != "" {
			if _, err := upd.ExecContext(ctx, it.Priority, it.Source, it.ExtID, it.Priority); err != nil {
				return 0, fmt.Errorf("update priority %s/%s: %w", it.Source, it.ExtID, err)
			}
		}
	}
	return n, tx.Commit()
}

// Unscored returns items fetched after since that still need a score.
func (s *Store) Unscored(ctx context.Context, since time.Time, limit int) ([]Item, error) {
	return s.query(ctx, `WHERE score IS NULL AND score_attempts < ? AND score_retries < ? AND fetched_at > ?
		ORDER BY id LIMIT ?`, MaxScoreAttempts, MaxScoreRetries, since.Unix(), limit)
}

// CountScoreRetry counts a transient scoring failure for every id.
func (s *Store) CountScoreRetry(ctx context.Context, ids []int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, `UPDATE items SET score_retries = score_retries + 1 WHERE id = ?`, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SaveScores stores scores and counts a failed attempt for every id in
// attempted that did not get a score.
func (s *Store) SaveScores(ctx context.Context, attempted []int64, scores []Score) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, id := range attempted {
		if _, err := tx.ExecContext(ctx, `UPDATE items SET score_attempts = score_attempts + 1 WHERE id = ?`, id); err != nil {
			return err
		}
	}
	for _, sc := range scores {
		if _, err := tx.ExecContext(ctx, `UPDATE items SET score = ?, summary = ?, duplicate = ? WHERE id = ?`,
			sc.Score, sc.Summary, sc.Duplicate, sc.ID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// PendingUrgent returns ping items and high-scoring, non-duplicate items
// not sent yet, ping items first.
func (s *Store) PendingUrgent(ctx context.Context, minScore int, since time.Time, limit int) ([]Item, error) {
	return s.query(ctx, `WHERE (priority = 'ping' OR (score >= ? AND duplicate = 0))
		AND urgent_sent_at IS NULL AND fetched_at > ?
		ORDER BY priority = 'ping' DESC, score DESC, id LIMIT ?`, minScore, since.Unix(), limit)
}

// RecentUrgentTitles returns titles of items pinged after since, so the model
// can flag repeats of the same story.
func (s *Store) RecentUrgentTitles(ctx context.Context, since time.Time) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT title FROM items WHERE urgent_sent_at > ? ORDER BY urgent_sent_at`, since.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// MarkUrgentSent records that an urgent ping went out.
func (s *Store) MarkUrgentSent(ctx context.Context, id int64, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE items SET urgent_sent_at = ? WHERE id = ?`, now.Unix(), id)
	return err
}

// PendingDigest returns digest items and items scoring at least minScore
// that were not in a digest yet, digest items first.
func (s *Store) PendingDigest(ctx context.Context, minScore int, since time.Time, limit int) ([]Item, error) {
	return s.query(ctx, `WHERE (priority = 'digest' OR score >= ?) AND digest_sent_at IS NULL AND fetched_at > ?
		ORDER BY priority = 'digest' DESC, score DESC, id LIMIT ?`, minScore, since.Unix(), limit)
}

// MarkDigestSent records that items went out in a digest.
func (s *Store) MarkDigestSent(ctx context.Context, ids []int64, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, `UPDATE items SET digest_sent_at = ? WHERE id = ?`, now.Unix(), id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Prune deletes items fetched before cutoff.
func (s *Store) Prune(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM items WHERE fetched_at < ?`, cutoff.Unix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Get returns the value for key, or "" if unset.
func (s *Store) Get(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM kv WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// Set stores value under key.
func (s *Store) Set(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO kv (key, value) VALUES (?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

func (s *Store) query(ctx context.Context, where string, args ...any) ([]Item, error) {
	q := `SELECT id, source, ext_id, title, url, discuss_url, author, text, published_at,
		COALESCE(score, 0), summary, priority FROM items ` + strings.TrimSpace(where)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Item
	for rows.Next() {
		var it Item
		var pub int64
		if err := rows.Scan(&it.ID, &it.Source, &it.ExtID, &it.Title, &it.URL, &it.DiscussURL,
			&it.Author, &it.Text, &pub, &it.Score, &it.Summary, &it.Priority); err != nil {
			return nil, err
		}
		if pub > 0 {
			it.Published = time.Unix(pub, 0).UTC()
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

func unix(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}
