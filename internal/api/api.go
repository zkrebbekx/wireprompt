// Package api serves the wireprompt HTTP surface: the JSON API, the live SSE
// feed and the embedded web UI, mounted alongside the capture proxy.
package api

import (
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zkrebbekx/wireprompt/internal/capture"
	"github.com/zkrebbekx/wireprompt/internal/pricing"
	"github.com/zkrebbekx/wireprompt/internal/store"
)

//go:embed index.html
var indexHTML []byte

// Feed fans captured records out to live SSE subscribers.
type Feed struct {
	mu   sync.Mutex
	subs map[chan store.Record]struct{}
}

// NewFeed returns an empty feed.
func NewFeed() *Feed {
	return &Feed{subs: make(map[chan store.Record]struct{})}
}

// Publish sends a record to all subscribers, dropping it for slow ones.
func (f *Feed) Publish(r store.Record) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for ch := range f.subs {
		select {
		case ch <- r:
		default:
		}
	}
}

func (f *Feed) subscribe() chan store.Record {
	ch := make(chan store.Record, 64)
	f.mu.Lock()
	f.subs[ch] = struct{}{}
	f.mu.Unlock()
	return ch
}

func (f *Feed) unsubscribe(ch chan store.Record) {
	f.mu.Lock()
	delete(f.subs, ch)
	f.mu.Unlock()
}

// Server exposes the API over a store, feed, proxy (for replays) and the
// pricing table (context windows for the UI gauge).
type Server struct {
	store   *store.Store
	feed    *Feed
	proxy   *capture.Proxy
	pricing *pricing.Table
}

// New builds the API server.
func New(st *store.Store, feed *Feed, proxy *capture.Proxy, table *pricing.Table) *Server {
	return &Server{store: st, feed: feed, proxy: proxy, pricing: table}
}

// Register mounts all API and UI routes on mux.
func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.HandleFunc("GET /api/requests", s.handleList)
	mux.HandleFunc("GET /api/requests/{id}", s.handleGet)
	mux.HandleFunc("POST /api/requests/{id}/replay", s.handleReplay)
	mux.HandleFunc("GET /api/sessions", s.handleSessions)
	mux.HandleFunc("GET /api/stats", s.handleStats)
	mux.HandleFunc("GET /api/meta", s.handleMeta)
	mux.HandleFunc("GET /api/live", s.handleLive)
}

// Secure wraps a handler with wireprompt's access policy. With a token set,
// every request must present it (X-Wireprompt-Token header or ?token=).
// Without one, the Host header must be a localhost name — this blocks DNS
// rebinding attacks that would otherwise let a malicious webpage read the
// prompt database off 127.0.0.1.
func Secure(next http.Handler, token string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token != "" {
			got := r.Header.Get("X-Wireprompt-Token")
			if got == "" {
				got = r.URL.Query().Get("token")
			}
			if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
				http.Error(w, "missing or invalid token", http.StatusUnauthorized)
				return
			}
		} else if !isLocalHost(r.Host) {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		// CSRF guard for state-changing endpoints (replay spends real API
		// money): browsers stamp cross-site requests with
		// Sec-Fetch-Site: cross-site; CLI clients don't send the header.
		if r.Method != http.MethodGet {
			if sfs := r.Header.Get("Sec-Fetch-Site"); sfs == "cross-site" {
				http.Error(w, "cross-site request blocked", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func isLocalHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if host == "localhost" || host == "::1" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return true
	}
	return false
}

func (s *Server) handleIndex(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(indexHTML)
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]string{"status": "ok", "app": "wireprompt"})
}

func parseSince(v string) time.Time {
	if v == "" {
		return time.Time{}
	}
	if d, err := time.ParseDuration(v); err == nil {
		return time.Now().Add(-d)
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t
	}
	return time.Time{}
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	beforeID, _ := strconv.ParseInt(q.Get("before_id"), 10, 64)
	recs, err := s.store.List(store.ListOptions{
		Session:  q.Get("session"),
		Model:    q.Get("model"),
		Provider: q.Get("provider"),
		Status:   q.Get("status"),
		Query:    q.Get("q"),
		Since:    parseSince(q.Get("since")),
		BeforeID: beforeID,
		Limit:    limit,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if recs == nil {
		recs = []store.Record{}
	}
	writeJSON(w, recs)
}

// Composition estimates where a request's input tokens went, scaled to the
// reported token counts from a chars/4 heuristic over the request body parts.
type Composition struct {
	SystemTokens   int64 `json:"system_tokens"`
	ToolsTokens    int64 `json:"tools_tokens"`
	HistoryTokens  int64 `json:"history_tokens"`
	LastTurnTokens int64 `json:"last_turn_tokens"`
}

func estimateComposition(body []byte, totalInput int64) *Composition {
	var req struct {
		System            json.RawMessage   `json:"system"`
		SystemInstruction json.RawMessage   `json:"systemInstruction"` // gemini
		Instructions      json.RawMessage   `json:"instructions"`      // responses API
		Tools             json.RawMessage   `json:"tools"`
		Messages          []json.RawMessage `json:"messages"`
		Input             []json.RawMessage `json:"input"`    // responses API
		Contents          []json.RawMessage `json:"contents"` // gemini
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return nil
	}
	msgs := req.Messages
	if len(msgs) == 0 {
		msgs = req.Input
	}
	if len(msgs) == 0 {
		msgs = req.Contents
	}
	if len(msgs) == 0 && req.System == nil && req.SystemInstruction == nil {
		return nil
	}
	chars := func(raw json.RawMessage) int64 { return int64(len(raw)) }
	var sys, tools, hist, last int64
	sys = chars(req.System) + chars(req.SystemInstruction) + chars(req.Instructions)
	tools = chars(req.Tools)
	for i, m := range msgs {
		// OpenAI chat carries the system prompt as a system/developer-role
		// message; attribute it to the system bucket.
		var role struct {
			Role string `json:"role"`
		}
		if json.Unmarshal(m, &role) == nil && (role.Role == "system" || role.Role == "developer") {
			sys += chars(m)
			continue
		}
		if i == len(msgs)-1 {
			last = chars(m)
		} else {
			hist += chars(m)
		}
	}
	total := sys + tools + hist + last
	if total == 0 {
		return nil
	}
	c := &Composition{}
	// Scale char shares onto the real token total when known; otherwise use
	// the chars/4 heuristic directly.
	scale := func(part int64) int64 {
		if totalInput > 0 {
			return int64(float64(part) / float64(total) * float64(totalInput))
		}
		return part / 4
	}
	c.SystemTokens = scale(sys)
	c.ToolsTokens = scale(tools)
	c.HistoryTokens = scale(hist)
	c.LastTurnTokens = scale(last)
	return c
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	rec, err := s.store.Get(id)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	type detail struct {
		store.Record
		RequestBodyText  string       `json:"request_body_text"`
		ResponseBodyText string       `json:"response_body_text"`
		Composition      *Composition `json:"composition,omitempty"`
		PrevID           int64        `json:"prev_id,omitempty"`
	}
	totalInput := rec.InputTokens + rec.CacheReadTokens + rec.CacheWriteTokens + rec.CacheWrite1hDup
	d := detail{
		Record:           *rec,
		RequestBodyText:  string(rec.RequestBody),
		ResponseBodyText: string(rec.ResponseBody),
		Composition:      estimateComposition(rec.RequestBody, totalInput),
	}
	if prev, err := s.store.PrevInSession(rec.Session, rec.ID); err == nil {
		d.PrevID = prev.ID
	}
	d.RequestBody, d.ResponseBody = nil, nil
	writeJSON(w, d)
}

func (s *Server) handleReplay(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	// Optional JSON body {"request_body": "..."} — the edit-and-resend
	// workbench replaces the stored request body for this replay only.
	var override []byte
	var payload struct {
		RequestBody string `json:"request_body"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(&payload); err == nil && payload.RequestBody != "" {
		override = []byte(payload.RequestBody)
	}
	rec, err := s.proxy.Replay(id, override)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, rec)
}

// handleMeta exposes the resolved pricing table (including context windows)
// so the UI gauge uses the same overridable data as cost computation.
func (s *Server) handleMeta(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{"pricing": s.pricing})
}

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	rows, err := s.store.Sessions(parseSince(r.URL.Query().Get("since")), limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if rows == nil {
		rows = []store.SessionRow{}
	}
	writeJSON(w, rows)
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.Stats(r.URL.Query().Get("by"), parseSince(r.URL.Query().Get("since")))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if rows == nil {
		rows = []store.StatRow{}
	}
	writeJSON(w, rows)
}

func (s *Server) handleLive(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	ch := s.feed.subscribe()
	defer s.feed.unsubscribe(ch)

	keepalive := time.NewTicker(20 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepalive.C:
			w.Write([]byte(": keepalive\n\n"))
			flusher.Flush()
		case rec := <-ch:
			data, err := json.Marshal(rec)
			if err != nil {
				continue
			}
			w.Write([]byte("data: "))
			w.Write(data)
			w.Write([]byte("\n\n"))
			flusher.Flush()
		}
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
