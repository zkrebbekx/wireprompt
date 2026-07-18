// Package capture implements the recording reverse proxy. It forwards LLM API
// traffic byte-for-byte (including SSE streams) while teeing both directions
// into the store with token usage, cost, latency and TTFT attached.
package capture

import (
	"bytes"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/zkrebbekx/wireprompt/internal/pricing"
	"github.com/zkrebbekx/wireprompt/internal/provider"
	"github.com/zkrebbekx/wireprompt/internal/store"
)

// maxStoredBody caps how much of each body is persisted. Larger bodies are
// still proxied in full; only the stored copy is truncated.
const maxStoredBody = 16 << 20 // 16 MiB

// Route maps a path prefix (e.g. "anthropic") to an upstream base URL and the
// wire format used to parse its traffic.
type Route struct {
	Name     string
	Upstream *url.URL
	Format   provider.Format
}

// Proxy records traffic for a set of provider routes.
type Proxy struct {
	routes  map[string]Route
	store   *store.Store
	pricing *pricing.Table
	notify  func(store.Record)
}

// New builds a proxy. notify (optional) fires after each captured request with
// the stored record, bodies omitted — used for the live feed.
func New(st *store.Store, table *pricing.Table, routes []Route, notify func(store.Record)) *Proxy {
	m := make(map[string]Route, len(routes))
	for _, r := range routes {
		m[r.Name] = r
	}
	return &Proxy{routes: m, store: st, pricing: table, notify: notify}
}

// DefaultRoutes returns the built-in Anthropic and OpenAI routes plus any
// extra openai-compatible upstreams (name → base URL).
func DefaultRoutes(extra map[string]string) ([]Route, error) {
	anthropic, _ := url.Parse("https://api.anthropic.com")
	openai, _ := url.Parse("https://api.openai.com")
	routes := []Route{
		{Name: "anthropic", Upstream: anthropic, Format: provider.FormatAnthropic},
		{Name: "openai", Upstream: openai, Format: provider.FormatOpenAI},
	}
	for name, base := range extra {
		u, err := url.Parse(base)
		if err != nil {
			return nil, err
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

func (p *Proxy) forward(w http.ResponseWriter, r *http.Request, session string, route Route, rest string) {
	started := time.Now()

	reqBody, err := io.ReadAll(io.LimitReader(r.Body, maxStoredBody+1))
	if err != nil {
		http.Error(w, "read request: "+err.Error(), http.StatusBadGateway)
		return
	}
	r.Body.Close()

	target := *route.Upstream
	target.Path = strings.TrimSuffix(target.Path, "/") + rest
	target.RawQuery = r.URL.RawQuery

	out, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), bytes.NewReader(reqBody))
	if err != nil {
		http.Error(w, "build upstream request: "+err.Error(), http.StatusBadGateway)
		return
	}
	out.Header = r.Header.Clone()
	out.Header.Del("Accept-Encoding") // keep stored bodies uncompressed
	out.Host = target.Host

	resp, err := http.DefaultTransport.RoundTrip(out)
	if err != nil {
		http.Error(w, "upstream: "+err.Error(), http.StatusBadGateway)
		p.record(store.Record{
			StartedAt: started, DurationMS: time.Since(started).Milliseconds(),
			Session: session, Provider: route.Name, Method: r.Method, Path: rest,
			Status: http.StatusBadGateway, RequestBody: clip(reqBody),
			Model: provider.RequestModel(reqBody), Error: err.Error(),
		})
		return
	}
	defer resp.Body.Close()

	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)

	// Stream upstream → client while teeing into a capped buffer. TTFT is
	// measured at the first byte forwarded.
	var respBuf bytes.Buffer
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
			if respBuf.Len() < maxStoredBody {
				respBuf.Write(buf[:n])
			}
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
	usage := provider.Parse(route.Format, respBuf.Bytes(), sse)
	if usage.Model == "" {
		usage.Model = provider.RequestModel(reqBody)
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
		CacheWriteTokens: usage.CacheWriteTokens,
		CostUSD:          p.pricing.Cost(usage.Model, usage.InputTokens, usage.OutputTokens, usage.CacheReadTokens, usage.CacheWriteTokens),
		RequestBody:      clip(reqBody),
		ResponseBody:     clip(respBuf.Bytes()),
	}
	if copyErr != nil {
		rec.Error = copyErr.Error()
	}
	p.record(rec)
}

func (p *Proxy) record(r store.Record) {
	if err := p.store.Insert(&r); err != nil {
		log.Printf("wireprompt: store insert failed: %v", err)
		return
	}
	if p.notify != nil {
		r.RequestBody, r.ResponseBody = nil, nil
		p.notify(r)
	}
}

func clip(b []byte) []byte {
	if len(b) > maxStoredBody {
		return b[:maxStoredBody]
	}
	return b
}
