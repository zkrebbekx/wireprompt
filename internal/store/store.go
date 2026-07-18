// Package store persists captured requests in a local SQLite database.
package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	CacheWriteTokens int64     `json:"cache_write_tokens"` // 5m tier (or unsplit legacy)
	CacheWrite1hDup  int64     `json:"cache_write_1h_tokens"`
	ToolCalls        int64     `json:"tool_calls"`
	ToolNames        []string  `json:"tool_names,omitempty"`
	CostUSD          float64   `json:"cost_usd"`
	SavedUSD         float64   `json:"saved_usd"`
	Priced           bool      `json:"priced"`
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
	SavedUSD         float64 `json:"saved_usd"`
	Unpriced         int64   `json:"unpriced"`
}

// SessionRow is one session's rollup.
type SessionRow struct {
	Session          string           `json:"session"`
	Requests         int64            `json:"requests"`
	FirstAt          time.Time        `json:"first_at"`
	LastAt           time.Time        `json:"last_at"`
	InputTokens      int64            `json:"input_tokens"`
	OutputTokens     int64            `json:"output_tokens"`
	CacheReadTokens  int64            `json:"cache_read_tokens"`
	CacheWriteTokens int64            `json:"cache_write_tokens"`
	CostUSD          float64          `json:"cost_usd"`
	SavedUSD         float64          `json:"saved_usd"`
	Models           []string         `json:"models"`
	ToolCounts       map[string]int64 `json:"tool_counts,omitempty"`
	CacheBustIDs     []int64          `json:"cache_bust_ids,omitempty"`
}

// Store wraps the SQLite database.
type Store struct {
	db  *sql.DB
	fts bool
}

// DefaultPath returns the standard database location, creating its directory
// with owner-only permissions.
func DefaultPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".wireprompt")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	os.Chmod(dir, 0o700)
	return filepath.Join(dir, "wireprompt.db"), nil
}

const schemaV2 = `
CREATE TABLE IF NOT EXISTS requests (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	started_at_ms INTEGER NOT NULL,
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
	cache_write_1h_tokens INTEGER NOT NULL DEFAULT 0,
	tool_calls INTEGER NOT NULL DEFAULT 0,
	tool_names TEXT NOT NULL DEFAULT '[]',
	cost_usd REAL NOT NULL DEFAULT 0,
	saved_usd REAL NOT NULL DEFAULT 0,
	priced INTEGER NOT NULL DEFAULT 1,
	request_body BLOB,
	response_body BLOB,
	error TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_requests_started ON requests(started_at_ms);
CREATE INDEX IF NOT EXISTS idx_requests_session_id ON requests(session, id);
CREATE INDEX IF NOT EXISTS idx_requests_model ON requests(model);
`

const ftsSchema = `
CREATE VIRTUAL TABLE IF NOT EXISTS requests_fts USING fts5(
	request_body, response_body, content='requests', content_rowid='id'
);
CREATE TRIGGER IF NOT EXISTS requests_fts_ai AFTER INSERT ON requests BEGIN
	INSERT INTO requests_fts(rowid, request_body, response_body)
	VALUES (new.id, new.request_body, new.response_body);
END;
CREATE TRIGGER IF NOT EXISTS requests_fts_ad AFTER DELETE ON requests BEGIN
	INSERT INTO requests_fts(requests_fts, rowid, request_body, response_body)
	VALUES ('delete', old.id, old.request_body, old.response_body);
END;
`

// Open opens (creating or migrating if needed) the database at path.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	// modernc sqlite serializes writes; a single connection avoids
	// SQLITE_BUSY under concurrent captures.
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	// FTS5 is compiled into modernc sqlite; if this ever fails we fall back
	// to LIKE-based search transparently.
	if _, err := db.Exec(ftsSchema); err == nil {
		s.fts = true
	}
	// The database holds prompt bodies — owner-only.
	os.Chmod(path, 0o600)
	return s, nil
}

func (s *Store) migrate() error {
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version >= 2 {
		return nil
	}
	// Detect a v1 database: requests table exists with TEXT started_at.
	var hasV1 bool
	var ddl string
	err := s.db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name='requests'`).Scan(&ddl)
	if err == nil && strings.Contains(ddl, "started_at TEXT") {
		hasV1 = true
	}
	if hasV1 {
		if _, err := s.db.Exec("ALTER TABLE requests RENAME TO requests_v1"); err != nil {
			return fmt.Errorf("migrate v1→v2: %w", err)
		}
	}
	if _, err := s.db.Exec(schemaV2); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	if hasV1 {
		if err := s.copyV1(); err != nil {
			return fmt.Errorf("migrate v1 rows: %w", err)
		}
		if _, err := s.db.Exec("DROP TABLE requests_v1"); err != nil {
			return err
		}
	}
	_, err = s.db.Exec("PRAGMA user_version = 2")
	return err
}

func (s *Store) copyV1() error {
	rows, err := s.db.Query(`SELECT started_at, duration_ms, ttft_ms, session,
		provider, model, method, path, status, streamed, input_tokens,
		output_tokens, cache_read_tokens, cache_write_tokens, cost_usd,
		request_body, response_body, error FROM requests_v1 ORDER BY id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	type v1row struct {
		started                                       string
		durMS, ttftMS                                 int64
		session, provider, model, method, path, errS  string
		status                                        int
		streamed                                      bool
		in, out, cr, cw                               int64
		cost                                          float64
		reqB, respB                                   []byte
	}
	var all []v1row
	for rows.Next() {
		var r v1row
		if err := rows.Scan(&r.started, &r.durMS, &r.ttftMS, &r.session, &r.provider,
			&r.model, &r.method, &r.path, &r.status, &r.streamed, &r.in, &r.out,
			&r.cr, &r.cw, &r.cost, &r.reqB, &r.respB, &r.errS); err != nil {
			return err
		}
		all = append(all, r)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, r := range all {
		t, _ := time.Parse(time.RFC3339Nano, r.started)
		if _, err := s.db.Exec(`INSERT INTO requests (started_at_ms, duration_ms,
			ttft_ms, session, provider, model, method, path, status, streamed,
			input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
			cost_usd, request_body, response_body, error)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			t.UnixMilli(), r.durMS, r.ttftMS, r.session, r.provider, r.model,
			r.method, r.path, r.status, r.streamed, r.in, r.out, r.cr, r.cw,
			r.cost, r.reqB, r.respB, r.errS); err != nil {
			return err
		}
	}
	return nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

func marshalTools(names []string) string {
	if len(names) == 0 {
		return "[]"
	}
	b, err := json.Marshal(names)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// Insert persists a record and sets its ID.
func (s *Store) Insert(r *Record) error {
	res, err := s.db.Exec(`INSERT INTO requests
		(started_at_ms, duration_ms, ttft_ms, session, provider, model, method,
		 path, status, streamed, input_tokens, output_tokens, cache_read_tokens,
		 cache_write_tokens, cache_write_1h_tokens, tool_calls, tool_names,
		 cost_usd, saved_usd, priced, request_body, response_body, error)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.StartedAt.UnixMilli(), r.DurationMS, r.TTFTMS,
		r.Session, r.Provider, r.Model, r.Method, r.Path,
		r.Status, r.Streamed, r.InputTokens, r.OutputTokens, r.CacheReadTokens,
		r.CacheWriteTokens, r.CacheWrite1hDup, r.ToolCalls, marshalTools(r.ToolNames),
		r.CostUSD, r.SavedUSD, r.Priced, r.RequestBody, r.ResponseBody, r.Error)
	if err != nil {
		return err
	}
	r.ID, err = res.LastInsertId()
	return err
}

// ListOptions filters List results.
type ListOptions struct {
	Session  string
	Model    string
	Provider string
	Status   string // "ok" (2xx) or "err" (non-2xx)
	Query    string // full-text search over bodies
	Since    time.Time
	Limit    int
}

const listCols = `id, started_at_ms, duration_ms, ttft_ms, session, provider,
	model, method, path, status, streamed, input_tokens, output_tokens,
	cache_read_tokens, cache_write_tokens, cache_write_1h_tokens, tool_calls,
	tool_names, cost_usd, saved_usd, priced, error`

func scanListRow(rows interface{ Scan(...any) error }) (Record, error) {
	var r Record
	var startedMS int64
	var toolNames string
	err := rows.Scan(&r.ID, &startedMS, &r.DurationMS, &r.TTFTMS, &r.Session,
		&r.Provider, &r.Model, &r.Method, &r.Path, &r.Status, &r.Streamed,
		&r.InputTokens, &r.OutputTokens, &r.CacheReadTokens, &r.CacheWriteTokens,
		&r.CacheWrite1hDup, &r.ToolCalls, &toolNames, &r.CostUSD, &r.SavedUSD,
		&r.Priced, &r.Error)
	if err != nil {
		return r, err
	}
	r.StartedAt = time.UnixMilli(startedMS).UTC()
	json.Unmarshal([]byte(toolNames), &r.ToolNames)
	return r, nil
}

// List returns recent records without bodies, newest first.
func (s *Store) List(o ListOptions) ([]Record, error) {
	if o.Limit <= 0 || o.Limit > 1000 {
		o.Limit = 200
	}
	q := "SELECT " + listCols + " FROM requests WHERE 1=1"
	args := []any{}
	if o.Session != "" {
		q += " AND session = ?"
		args = append(args, o.Session)
	}
	if o.Model != "" {
		q += " AND model LIKE ?"
		args = append(args, o.Model+"%")
	}
	if o.Provider != "" {
		q += " AND provider = ?"
		args = append(args, o.Provider)
	}
	switch o.Status {
	case "ok":
		q += " AND status BETWEEN 200 AND 299"
	case "err":
		q += " AND (status < 200 OR status >= 300)"
	}
	if !o.Since.IsZero() {
		q += " AND started_at_ms >= ?"
		args = append(args, o.Since.UnixMilli())
	}
	if o.Query != "" {
		if s.fts {
			q += ` AND id IN (SELECT rowid FROM requests_fts WHERE requests_fts MATCH ?)`
			args = append(args, ftsQuote(o.Query))
		} else {
			q += ` AND (request_body LIKE ? OR response_body LIKE ?)`
			pat := "%" + o.Query + "%"
			args = append(args, pat, pat)
		}
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
		r, err := scanListRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ftsQuote wraps each whitespace token in double quotes so user input never
// hits FTS5 query-syntax errors.
func ftsQuote(q string) string {
	fields := strings.Fields(q)
	for i, f := range fields {
		fields[i] = `"` + strings.ReplaceAll(f, `"`, ``) + `"`
	}
	return strings.Join(fields, " ")
}

// Get returns one record including bodies.
func (s *Store) Get(id int64) (*Record, error) {
	row := s.db.QueryRow("SELECT "+listCols+", request_body, response_body FROM requests WHERE id = ?", id)
	var r Record
	var startedMS int64
	var toolNames string
	err := row.Scan(&r.ID, &startedMS, &r.DurationMS, &r.TTFTMS, &r.Session,
		&r.Provider, &r.Model, &r.Method, &r.Path, &r.Status, &r.Streamed,
		&r.InputTokens, &r.OutputTokens, &r.CacheReadTokens, &r.CacheWriteTokens,
		&r.CacheWrite1hDup, &r.ToolCalls, &toolNames, &r.CostUSD, &r.SavedUSD,
		&r.Priced, &r.Error, &r.RequestBody, &r.ResponseBody)
	if err != nil {
		return nil, err
	}
	r.StartedAt = time.UnixMilli(startedMS).UTC()
	json.Unmarshal([]byte(toolNames), &r.ToolNames)
	return &r, nil
}

// PrevInSession returns the most recent record in the same session before id,
// with bodies — used for turn-delta computation.
func (s *Store) PrevInSession(session string, beforeID int64) (*Record, error) {
	var id int64
	err := s.db.QueryRow(`SELECT id FROM requests WHERE session = ? AND id < ?
		ORDER BY id DESC LIMIT 1`, session, beforeID).Scan(&id)
	if err != nil {
		return nil, err
	}
	return s.Get(id)
}

// Stats aggregates by "model", "session" or "day" since the given time
// (zero time = all history).
func (s *Store) Stats(groupBy string, since time.Time) ([]StatRow, error) {
	var key string
	switch groupBy {
	case "session":
		key = "session"
	case "day":
		key = "date(started_at_ms/1000, 'unixepoch')"
	default:
		key = "model"
	}
	q := fmt.Sprintf(`SELECT %s, COUNT(*), SUM(input_tokens), SUM(output_tokens),
		SUM(cache_read_tokens), SUM(cache_write_tokens + cache_write_1h_tokens),
		SUM(cost_usd), SUM(saved_usd), SUM(1 - priced)
		FROM requests WHERE started_at_ms >= ? GROUP BY 1 ORDER BY 7 DESC`, key)
	rows, err := s.db.Query(q, sinceMS(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StatRow
	for rows.Next() {
		var r StatRow
		if err := rows.Scan(&r.Key, &r.Requests, &r.InputTokens, &r.OutputTokens,
			&r.CacheReadTokens, &r.CacheWriteTokens, &r.CostUSD, &r.SavedUSD,
			&r.Unpriced); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func sinceMS(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

// Sessions returns per-session rollups, most recent activity first.
func (s *Store) Sessions(since time.Time, limit int) ([]SessionRow, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.Query(`SELECT session, COUNT(*), MIN(started_at_ms),
		MAX(started_at_ms), SUM(input_tokens), SUM(output_tokens),
		SUM(cache_read_tokens), SUM(cache_write_tokens + cache_write_1h_tokens),
		SUM(cost_usd), SUM(saved_usd)
		FROM requests WHERE started_at_ms >= ?
		GROUP BY session ORDER BY MAX(started_at_ms) DESC LIMIT ?`,
		sinceMS(since), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SessionRow
	for rows.Next() {
		var r SessionRow
		var firstMS, lastMS int64
		if err := rows.Scan(&r.Session, &r.Requests, &firstMS, &lastMS,
			&r.InputTokens, &r.OutputTokens, &r.CacheReadTokens,
			&r.CacheWriteTokens, &r.CostUSD, &r.SavedUSD); err != nil {
			return nil, err
		}
		r.FirstAt = time.UnixMilli(firstMS).UTC()
		r.LastAt = time.UnixMilli(lastMS).UTC()
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		if err := s.enrichSession(&out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// enrichSession fills models, tool counts and cache-bust detection from the
// session's individual rows.
func (s *Store) enrichSession(row *SessionRow) error {
	rows, err := s.db.Query(`SELECT id, model, tool_names, cache_read_tokens,
		input_tokens FROM requests WHERE session = ? ORDER BY id LIMIT 2000`,
		row.Session)
	if err != nil {
		return err
	}
	defer rows.Close()
	models := map[string]bool{}
	tools := map[string]int64{}
	var prevCacheRead int64
	for rows.Next() {
		var id, cacheRead, input int64
		var model, toolNames string
		if err := rows.Scan(&id, &model, &toolNames, &cacheRead, &input); err != nil {
			return err
		}
		if model != "" {
			models[model] = true
		}
		var names []string
		json.Unmarshal([]byte(toolNames), &names)
		for _, n := range names {
			tools[n]++
		}
		// Cache bust: previous request read cache, this sizeable request
		// read none — something invalidated the prefix.
		if prevCacheRead > 0 && cacheRead == 0 && input > 1000 {
			row.CacheBustIDs = append(row.CacheBustIDs, id)
		}
		prevCacheRead = cacheRead
	}
	for m := range models {
		row.Models = append(row.Models, m)
	}
	if len(tools) > 0 {
		row.ToolCounts = tools
	}
	return rows.Err()
}

// Prune deletes records older than cutoff. When bodiesOnly is set, bodies are
// nulled instead of rows deleted. Returns affected row count.
func (s *Store) Prune(cutoff time.Time, bodiesOnly bool) (int64, error) {
	var res sql.Result
	var err error
	if bodiesOnly {
		res, err = s.db.Exec(`UPDATE requests SET request_body = NULL,
			response_body = NULL WHERE started_at_ms < ?`, cutoff.UnixMilli())
	} else {
		res, err = s.db.Exec(`DELETE FROM requests WHERE started_at_ms < ?`, cutoff.UnixMilli())
	}
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if _, err := s.db.Exec("VACUUM"); err != nil {
		return n, err
	}
	return n, nil
}
