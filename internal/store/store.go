// Package store persists captured requests in a local SQLite database.
package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// Record is one captured LLM API request.
type Record struct {
	ID               int64     `json:"id"`
	StartedAt        time.Time `json:"started_at"`
	DurationMS       int64     `json:"duration_ms"`
	TTFTMS           int64     `json:"ttft_ms"`
	Session          string    `json:"session"`
	Provider         string    `json:"provider"`
	Model            string    `json:"model"`
	Method           string    `json:"method"`
	Path             string    `json:"path"`
	Status           int       `json:"status"`
	Streamed         bool      `json:"streamed"`
	InputTokens      int64     `json:"input_tokens"`
	OutputTokens     int64     `json:"output_tokens"`
	CacheReadTokens  int64     `json:"cache_read_tokens"`
	CacheWriteTokens int64     `json:"cache_write_tokens"`
	CostUSD          float64   `json:"cost_usd"`
	RequestBody      []byte    `json:"request_body,omitempty"`
	ResponseBody     []byte    `json:"response_body,omitempty"`
	Error            string    `json:"error,omitempty"`
}

// StatRow is one aggregation bucket from Stats.
type StatRow struct {
	Key              string  `json:"key"`
	Requests         int64   `json:"requests"`
	InputTokens      int64   `json:"input_tokens"`
	OutputTokens     int64   `json:"output_tokens"`
	CacheReadTokens  int64   `json:"cache_read_tokens"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
	CostUSD          float64 `json:"cost_usd"`
}

// Store wraps the SQLite database.
type Store struct {
	db *sql.DB
}

// DefaultPath returns the standard database location, creating its directory.
func DefaultPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".wireprompt")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return filepath.Join(dir, "wireprompt.db"), nil
}

const schema = `
CREATE TABLE IF NOT EXISTS requests (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	started_at TEXT NOT NULL,
	duration_ms INTEGER NOT NULL DEFAULT 0,
	ttft_ms INTEGER NOT NULL DEFAULT 0,
	session TEXT NOT NULL DEFAULT '',
	provider TEXT NOT NULL DEFAULT '',
	model TEXT NOT NULL DEFAULT '',
	method TEXT NOT NULL DEFAULT '',
	path TEXT NOT NULL DEFAULT '',
	status INTEGER NOT NULL DEFAULT 0,
	streamed INTEGER NOT NULL DEFAULT 0,
	input_tokens INTEGER NOT NULL DEFAULT 0,
	output_tokens INTEGER NOT NULL DEFAULT 0,
	cache_read_tokens INTEGER NOT NULL DEFAULT 0,
	cache_write_tokens INTEGER NOT NULL DEFAULT 0,
	cost_usd REAL NOT NULL DEFAULT 0,
	request_body BLOB,
	response_body BLOB,
	error TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_requests_started ON requests(started_at);
CREATE INDEX IF NOT EXISTS idx_requests_session ON requests(session);
`

// Open opens (creating if needed) the database at path.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	// modernc sqlite serializes writes; a single connection avoids
	// SQLITE_BUSY under concurrent captures.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &Store{db: db}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// Insert persists a record and sets its ID.
func (s *Store) Insert(r *Record) error {
	res, err := s.db.Exec(`INSERT INTO requests
		(started_at, duration_ms, ttft_ms, session, provider, model, method, path,
		 status, streamed, input_tokens, output_tokens, cache_read_tokens,
		 cache_write_tokens, cost_usd, request_body, response_body, error)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.StartedAt.UTC().Format(time.RFC3339Nano), r.DurationMS, r.TTFTMS,
		r.Session, r.Provider, r.Model, r.Method, r.Path,
		r.Status, r.Streamed, r.InputTokens, r.OutputTokens, r.CacheReadTokens,
		r.CacheWriteTokens, r.CostUSD, r.RequestBody, r.ResponseBody, r.Error)
	if err != nil {
		return err
	}
	r.ID, err = res.LastInsertId()
	return err
}

// ListOptions filters List results.
type ListOptions struct {
	Session string
	Model   string
	Limit   int
}

// List returns recent records without bodies, newest first.
func (s *Store) List(o ListOptions) ([]Record, error) {
	if o.Limit <= 0 || o.Limit > 1000 {
		o.Limit = 200
	}
	q := `SELECT id, started_at, duration_ms, ttft_ms, session, provider, model,
		method, path, status, streamed, input_tokens, output_tokens,
		cache_read_tokens, cache_write_tokens, cost_usd, error
		FROM requests WHERE 1=1`
	args := []any{}
	if o.Session != "" {
		q += " AND session = ?"
		args = append(args, o.Session)
	}
	if o.Model != "" {
		q += " AND model LIKE ?"
		args = append(args, o.Model+"%")
	}
	q += " ORDER BY id DESC LIMIT ?"
	args = append(args, o.Limit)

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Record
	for rows.Next() {
		var r Record
		var started string
		if err := rows.Scan(&r.ID, &started, &r.DurationMS, &r.TTFTMS, &r.Session,
			&r.Provider, &r.Model, &r.Method, &r.Path, &r.Status, &r.Streamed,
			&r.InputTokens, &r.OutputTokens, &r.CacheReadTokens,
			&r.CacheWriteTokens, &r.CostUSD, &r.Error); err != nil {
			return nil, err
		}
		r.StartedAt, _ = time.Parse(time.RFC3339Nano, started)
		out = append(out, r)
	}
	return out, rows.Err()
}

// Get returns one record including bodies.
func (s *Store) Get(id int64) (*Record, error) {
	var r Record
	var started string
	err := s.db.QueryRow(`SELECT id, started_at, duration_ms, ttft_ms, session,
		provider, model, method, path, status, streamed, input_tokens,
		output_tokens, cache_read_tokens, cache_write_tokens, cost_usd,
		request_body, response_body, error
		FROM requests WHERE id = ?`, id).Scan(
		&r.ID, &started, &r.DurationMS, &r.TTFTMS, &r.Session, &r.Provider,
		&r.Model, &r.Method, &r.Path, &r.Status, &r.Streamed, &r.InputTokens,
		&r.OutputTokens, &r.CacheReadTokens, &r.CacheWriteTokens, &r.CostUSD,
		&r.RequestBody, &r.ResponseBody, &r.Error)
	if err != nil {
		return nil, err
	}
	r.StartedAt, _ = time.Parse(time.RFC3339Nano, started)
	return &r, nil
}

// Stats aggregates by "model", "session" or "day" since the given time
// (zero time = all history).
func (s *Store) Stats(groupBy string, since time.Time) ([]StatRow, error) {
	var key string
	switch groupBy {
	case "session":
		key = "session"
	case "day":
		key = "substr(started_at, 1, 10)"
	default:
		key = "model"
	}
	q := fmt.Sprintf(`SELECT %s, COUNT(*), SUM(input_tokens), SUM(output_tokens),
		SUM(cache_read_tokens), SUM(cache_write_tokens), SUM(cost_usd)
		FROM requests WHERE started_at >= ? GROUP BY 1 ORDER BY 7 DESC`, key)
	sinceStr := ""
	if !since.IsZero() {
		sinceStr = since.UTC().Format(time.RFC3339Nano)
	}
	rows, err := s.db.Query(q, sinceStr)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StatRow
	for rows.Next() {
		var r StatRow
		if err := rows.Scan(&r.Key, &r.Requests, &r.InputTokens, &r.OutputTokens,
			&r.CacheReadTokens, &r.CacheWriteTokens, &r.CostUSD); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
