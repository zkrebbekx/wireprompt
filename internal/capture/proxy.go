// Package capture implements the recording reverse proxy. It forwards LLM API
// traffic (including SSE streams) while teeing both directions into the store
// with token usage, cost, latency and TTFT attached.
package capture

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/zkrebbekx/wireprompt/internal/config"
	"github.com/zkrebbekx/wireprompt/internal/pricing"
	"github.com/zkrebbekx/wireprompt/internal/provider"
	"github.com/zkrebbekx/wireprompt/internal/store"
)

// maxStoredBody caps how much of each body is persisted. Larger bodies are
// still proxied in full; only the stored copy is truncated.
const maxStoredBody = 16 << 20 // 16 MiB

// rewriteLimit caps how large a request body may be to qualify for in-memory
// rewriting (usage injection). Larger bodies are streamed untouched.
const rewriteLimit = 8 << 20

// Route maps a path prefix (e.g. "anthropic") to an upstream base URL and the
// wire format used to parse its traffic.
type Route struct {
	Name     string
	Upstream *url.URL
	Format   provider.Format
}

// Proxy records traffic for a set of provider routes.
type Proxy struct {
	routes    map[string]Route
	store     *store.Store
	pricing   *pricing.Table
	cfg       *config.Config
	notify    func(store.Record)
	transport http.RoundTripper
}

// New builds a proxy. notify (optional) fires after each captured request with
// the stored record, bodies omitted — used for the live feed.
func New(st *store.Store, table *pricing.Table, cfg *config.Config, routes []Route, notify func(store.Record)) *Proxy {
	m := make(map[string]Route, len(routes))
	for _, r := range routes {
		m[r.Name] = r
	}
	// Agent tools fire many parallel requests at the same host; the default
	// transport's 2 idle conns per host churns connections.
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConnsPerHost = 32
	t.IdleConnTimeout = 120 * time.Second
	return &Proxy{routes: m, store: st, pricing: table, cfg: cfg, notify: notify, transport: t}
}

// DefaultRoutes returns the built-in Anthropic, OpenAI and Gemini routes plus
// any extra openai-compatible upstreams (name → base URL).
func DefaultRoutes(extra map[string]string) ([]Route, error) {
	anthropic, _ := url.Parse("https://api.anthropic.com")
	openai, _ := url.Parse("https://api.openai.com")
	gemini, _ := url.Parse("https://generativelanguage.googleapis.com")
	routes := []Route{
		{Name: "anthropic", Upstream: anthropic, Format: provider.FormatAnthropic},
		{Name: "openai", Upstream: openai, Format: provider.FormatOpenAI},
		{Name: "gemini", Upstream: gemini, Format: provider.FormatGemini},
	}
	for name, base := range extra {
		u, err := url.Parse(base)
		if err != nil {
			return nil, fmt.Errorf("upstream %s: %w", name, err)
		}
		routes = append(routes, Route{Name: name, Upstream: u, Format: provider.FormatOpenAI})
	}
	return routes, nil
}

// ServeHTTP handles /{provider}/... and /s/{session}/{provider}/... paths.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	session, routeName, rest, ok := splitPath(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	route, ok := p.routes[routeName]
	if !ok {
		http.NotFound(w, r)
		return
	}
	p.forward(w, r, session, route, rest)
}

// splitPath parses "/s/{session}/{provider}/rest" and "/{provider}/rest".
func splitPath(path string) (session, routeName, rest string, ok bool) {
	trimmed := strings.TrimPrefix(path, "/")
	parts := strings.SplitN(trimmed, "/", 4)
	if len(parts) >= 3 && parts[0] == "s" {
		rest = "/"
		if len(parts) == 4 {
			rest += parts[3]
		}
		return parts[1], parts[2], rest, true
	}
	if len(parts) >= 2 {
		return "default", parts[0], "/" + strings.Join(parts[1:], "/"), true
	}
	return "", "", "", false
}

// hopHeaders are connection-scoped and must not be forwarded (RFC 7230 §6.1).
var hopHeaders = []string{
	"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

func stripHopHeaders(h http.Header) {
	for _, f := range h.Values("Connection") {
		for _, name := range strings.Split(f, ",") {
			h.Del(strings.TrimSpace(name))
		}
	}
	for _, name := range hopHeaders {
		h.Del(name)
	}
}

// cappedBuffer keeps the first max bytes written and discards the rest,
// never returning an error (so it is safe as a TeeReader sink).
type cappedBuffer struct {
	buf bytes.Buffer
	max int
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if room := c.max - c.buf.Len(); room > 0 {
		if len(p) > room {
			c.buf.Write(p[:room])
		} else {
			c.buf.Write(p)
		}
	}
	return len(p), nil
}

func (p *Proxy) forward(w http.ResponseWriter, r *http.Request, session string, route Route, rest string) {
	started := time.Now()

	// Read up to rewriteLimit. If the whole body fits we can rewrite it
	// (usage injection); if not, stream the remainder untouched so large
	// uploads are never truncated.
	head, err := io.ReadAll(io.LimitReader(r.Body, rewriteLimit))
	if err != nil {
		http.Error(w, "read request: "+err.Error(), http.StatusBadGateway)
		return
	}
	var outBody io.Reader
	var outLen int64
	storedReq := &cappedBuffer{max: maxStoredBody}
	storedReq.Write(head)
	if int64(len(head)) < rewriteLimit {
		body := head
		if route.Format == provider.FormatOpenAI && p.cfg.InjectUsageEnabled() {
			body = provider.InjectStreamUsage(body)
			storedReq.buf.Reset()
			storedReq.Write(body)
		}
		outBody = bytes.NewReader(body)
		outLen = int64(len(body))
	} else {
		outBody = io.MultiReader(bytes.NewReader(head), io.TeeReader(r.Body, storedReq))
		outLen = r.ContentLength // may be -1 (chunked); passed through as-is
	}
	defer r.Body.Close()

	target := *route.Upstream
	target.Path = strings.TrimSuffix(target.Path, "/") + rest
	// Query params are forwarded but never stored: Gemini carries the API
	// key in ?key=.
	target.RawQuery = r.URL.RawQuery

	out, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), outBody)
	if err != nil {
		http.Error(w, "build upstream request: "+err.Error(), http.StatusBadGateway)
		return
	}
	out.Header = r.Header.Clone()
	stripHopHeaders(out.Header)
	out.Header.Del("Accept-Encoding") // keep stored bodies uncompressed
	out.ContentLength = outLen
	out.Host = target.Host

	resp, err := p.transport.RoundTrip(out)
	if err != nil {
		http.Error(w, "upstream: "+err.Error(), http.StatusBadGateway)
		p.record(store.Record{
			StartedAt: started, DurationMS: time.Since(started).Milliseconds(),
			Session: session, Provider: route.Name, Method: r.Method, Path: rest,
			Status: http.StatusBadGateway, RequestBody: storedReq.buf.Bytes(),
			Model: provider.RequestModel(storedReq.buf.Bytes()), Error: err.Error(),
		})
		return
	}
	defer resp.Body.Close()

	respHeader := resp.Header.Clone()
	stripHopHeaders(respHeader)
	for k, vv := range respHeader {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)

	// Stream upstream → client while teeing into a capped buffer. TTFT is
	// measured at the first byte forwarded.
	storedResp := &cappedBuffer{max: maxStoredBody}
	var ttft time.Duration
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	var copyErr error
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if ttft == 0 {
				ttft = time.Since(started)
			}
			storedResp.Write(buf[:n])
			if _, werr := w.Write(buf[:n]); werr != nil {
				copyErr = werr
				break
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			copyErr = rerr
			break
		}
	}

	sse := provider.IsSSE(resp.Header.Get("Content-Type"))
	usage := provider.Parse(route.Format, storedResp.buf.Bytes(), sse)
	if usage.Model == "" {
		usage.Model = provider.RequestModel(storedReq.buf.Bytes())
	}
	if usage.Model == "" && route.Format == provider.FormatGemini {
		usage.Model = provider.GeminiModelFromPath(rest)
	}

	cost, priced := p.pricing.Cost(usage.Model, usage.InputTokens, usage.OutputTokens,
		usage.CacheReadTokens, usage.CacheWrite5m, usage.CacheWrite1h)
	if usage.ProviderCostUSD > 0 {
		cost, priced = usage.ProviderCostUSD, true
	}

	rec := store.Record{
		StartedAt:        started,
		DurationMS:       time.Since(started).Milliseconds(),
		TTFTMS:           ttft.Milliseconds(),
		Session:          session,
		Provider:         route.Name,
		Model:            usage.Model,
		Method:           r.Method,
		Path:             rest,
		Status:           resp.StatusCode,
		Streamed:         sse,
		InputTokens:      usage.InputTokens,
		OutputTokens:     usage.OutputTokens,
		CacheReadTokens:  usage.CacheReadTokens,
		CacheWriteTokens: usage.CacheWrite5m,
		CacheWrite1hDup:  usage.CacheWrite1h,
		ToolCalls:        int64(len(usage.ToolNames)),
		ToolNames:        usage.ToolNames,
		CostUSD:          cost,
		SavedUSD:         p.pricing.Saved(usage.Model, usage.CacheReadTokens),
		Priced:           priced,
		RequestBody:      storedReq.buf.Bytes(),
		ResponseBody:     storedResp.buf.Bytes(),
	}
	if copyErr != nil {
		rec.Error = copyErr.Error()
	}
	p.record(rec)
}

func (p *Proxy) record(r store.Record) {
	if p.cfg.NoBodies {
		r.RequestBody, r.ResponseBody = nil, nil
	} else {
		r.RequestBody = p.cfg.ApplyRedaction(r.RequestBody)
		r.ResponseBody = p.cfg.ApplyRedaction(r.ResponseBody)
	}
	if err := p.store.Insert(&r); err != nil {
		log.Printf("wireprompt: store insert failed: %v", err)
		return
	}
	if p.notify != nil {
		r.RequestBody, r.ResponseBody = nil, nil
		p.notify(r)
	}
}

// replayEnvKeys maps route formats to the env var and header used for auth.
var replayAuth = map[provider.Format]struct{ env, header, prefix string }{
	provider.FormatAnthropic: {"ANTHROPIC_API_KEY", "x-api-key", ""},
	provider.FormatOpenAI:    {"OPENAI_API_KEY", "Authorization", "Bearer "},
	provider.FormatGemini:    {"GEMINI_API_KEY", "x-goog-api-key", ""},
}

// Replay re-sends a stored request to its upstream using API keys from the
// environment (keys are never stored). The replay is captured like any other
// request, under session "replay". Returns the new record.
func (p *Proxy) Replay(id int64) (*store.Record, error) {
	orig, err := p.store.Get(id)
	if err != nil {
		return nil, fmt.Errorf("load request %d: %w", id, err)
	}
	if len(orig.RequestBody) == 0 {
		return nil, fmt.Errorf("request %d has no stored body (no-bodies mode?)", id)
	}
	route, ok := p.routes[orig.Provider]
	if !ok {
		return nil, fmt.Errorf("no route for provider %q", orig.Provider)
	}
	auth, ok := replayAuth[route.Format]
	if !ok {
		return nil, fmt.Errorf("replay unsupported for provider %q", orig.Provider)
	}
	key := os.Getenv(auth.env)
	if key == "" {
		return nil, fmt.Errorf("set %s to replay %s requests (keys are never stored)", auth.env, orig.Provider)
	}

	req := httptestRequest(orig.Method, "/s/replay/"+orig.Provider+orig.Path, orig.RequestBody)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(auth.header, auth.prefix+key)
	if route.Format == provider.FormatAnthropic {
		req.Header.Set("anthropic-version", "2023-06-01")
	}

	rw := &captureResponseWriter{header: http.Header{}}
	p.ServeHTTP(rw, req)

	recs, err := p.store.List(store.ListOptions{Session: "replay", Limit: 1})
	if err != nil || len(recs) == 0 {
		return nil, fmt.Errorf("replay completed (status %d) but record not found", rw.status)
	}
	return &recs[0], nil
}

func httptestRequest(method, path string, body []byte) *http.Request {
	req, _ := http.NewRequest(method, "http://localhost"+path, bytes.NewReader(body))
	return req
}

// captureResponseWriter is a minimal ResponseWriter for internal replays.
type captureResponseWriter struct {
	header http.Header
	status int
	buf    bytes.Buffer
}

func (c *captureResponseWriter) Header() http.Header { return c.header }
func (c *captureResponseWriter) WriteHeader(s int)   { c.status = s }
func (c *captureResponseWriter) Write(p []byte) (int, error) {
	if c.buf.Len() < 1<<20 {
		c.buf.Write(p)
	}
	return len(p), nil
}
