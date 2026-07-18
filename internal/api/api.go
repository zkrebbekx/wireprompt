// Package api serves the wireprompt HTTP surface: the JSON API, the live SSE
// feed and the embedded web UI, mounted alongside the capture proxy.
package api

import (
	_ "embed"
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
	"time"

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

// Server exposes the API over a store and feed.
type Server struct {
	store *store.Store
	feed  *Feed
}

// New builds the API server.
func New(st *store.Store, feed *Feed) *Server {
	return &Server{store: st, feed: feed}
}

// Register mounts all API and UI routes on mux.
func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.HandleFunc("GET /api/requests", s.handleList)
	mux.HandleFunc("GET /api/requests/{id}", s.handleGet)
	mux.HandleFunc("GET /api/stats", s.handleStats)
	mux.HandleFunc("GET /api/live", s.handleLive)
}

func (s *Server) handleIndex(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(indexHTML)
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]string{"status": "ok", "app": "wireprompt"})
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	recs, err := s.store.List(store.ListOptions{
		Session: r.URL.Query().Get("session"),
		Model:   r.URL.Query().Get("model"),
		Limit:   limit,
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
	// Bodies are stored as raw bytes; expose them as strings for the UI.
	type detail struct {
		store.Record
		RequestBodyText  string `json:"request_body_text"`
		ResponseBodyText string `json:"response_body_text"`
	}
	d := detail{Record: *rec, RequestBodyText: string(rec.RequestBody), ResponseBodyText: string(rec.ResponseBody)}
	d.RequestBody, d.ResponseBody = nil, nil
	writeJSON(w, d)
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	var since time.Time
	if v := r.URL.Query().Get("since"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			since = time.Now().Add(-d)
		} else if t, err := time.Parse(time.RFC3339, v); err == nil {
			since = t
		}
	}
	rows, err := s.store.Stats(r.URL.Query().Get("by"), since)
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
