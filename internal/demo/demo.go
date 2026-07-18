// Package demo seeds the database with a realistic captured agent session so
// the UI has something to show before any real traffic arrives.
package demo

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/zkrebbekx/wireprompt/internal/pricing"
	"github.com/zkrebbekx/wireprompt/internal/store"
)

// Session is the session name demo records are created under.
const Session = "demo-agent-run"

type turn struct {
	userText   string
	assistant  string
	tools      []string
	outputTok  int64
	newInput   int64 // fresh (uncached) input this turn
	cacheRead  int64
	cacheWrite int64
	ttftMS     int64
	durMS      int64
	cacheBust  bool
}

var turns = []turn{
	{userText: "Add rate limiting to the /api/upload endpoint", assistant: "I'll look at the current handler first.", tools: []string{"Read"}, outputTok: 180, newInput: 9500, cacheWrite: 9200, ttftMS: 850, durMS: 4100},
	{userText: "[tool_result Read internal/api/upload.go]", assistant: "The handler has no limiter. I'll check how middleware is wired.", tools: []string{"Grep", "Read"}, outputTok: 210, newInput: 2100, cacheRead: 9200, cacheWrite: 1900, ttftMS: 610, durMS: 3800},
	{userText: "[tool_result Grep middleware]", assistant: "Found the chain in server.go. Writing a token-bucket limiter now.", tools: []string{"Write"}, outputTok: 940, newInput: 1700, cacheRead: 11100, cacheWrite: 1500, ttftMS: 590, durMS: 9200},
	{userText: "[tool_result Write internal/mw/ratelimit.go]", assistant: "Wiring it into the middleware chain.", tools: []string{"Edit"}, outputTok: 320, newInput: 1200, cacheRead: 12600, cacheWrite: 1100, ttftMS: 540, durMS: 4600},
	{userText: "[tool_result Edit server.go]", assistant: "Running the test suite to verify.", tools: []string{"Bash"}, outputTok: 90, newInput: 800, cacheRead: 13700, cacheWrite: 700, ttftMS: 520, durMS: 2200},
	// Cache bust: the harness rewrote the system prompt (e.g. mode switch)
	{userText: "[tool_result Bash go test ./... FAIL: TestUploadLimit]", assistant: "One test expected 429 headers. Fixing the limiter response.", tools: []string{"Edit"}, outputTok: 410, newInput: 15400, cacheWrite: 15100, ttftMS: 1900, durMS: 8300, cacheBust: true},
	{userText: "[tool_result Edit internal/mw/ratelimit.go]", assistant: "Re-running tests.", tools: []string{"Bash"}, outputTok: 70, newInput: 900, cacheRead: 15100, cacheWrite: 800, ttftMS: 480, durMS: 2100},
	{userText: "[tool_result Bash go test ./... ok]", assistant: "All tests pass. Rate limiting is live on /api/upload: token bucket, 10 req/s per client IP, 429 with Retry-After on excess. Files changed: internal/mw/ratelimit.go (new), server.go (wired into chain).", outputTok: 350, newInput: 700, cacheRead: 15900, cacheWrite: 600, ttftMS: 700, durMS: 5100},
}

const demoModel = "claude-opus-4-8"

// Seed inserts the demo session. Returns the number of records created.
func Seed(st *store.Store) (int, error) {
	table, err := pricing.Load()
	if err != nil {
		return 0, err
	}
	base := time.Now().Add(-9 * time.Minute)
	history := []map[string]any{}
	for i, t := range turns {
		history = append(history, map[string]any{"role": "user", "content": t.userText})
		reqBody, _ := json.Marshal(map[string]any{
			"model":      demoModel,
			"max_tokens": 32000,
			"stream":     true,
			"system": "You are a coding agent working in the acme-api repository. Follow the project style guide. Run tests before declaring success.",
			"tools": []map[string]any{
				{"name": "Read", "description": "Read a file"},
				{"name": "Write", "description": "Write a file"},
				{"name": "Edit", "description": "Edit a file"},
				{"name": "Grep", "description": "Search file contents"},
				{"name": "Bash", "description": "Run a shell command"},
			},
			"messages": history,
		})
		content := []map[string]any{{"type": "text", "text": t.assistant}}
		for _, tool := range t.tools {
			content = append(content, map[string]any{"type": "tool_use", "name": tool,
				"input": map[string]any{"path": "internal/..."}})
		}
		respBody, _ := json.Marshal(map[string]any{
			"model": demoModel, "content": content,
			"usage": map[string]any{
				"input_tokens": t.newInput, "output_tokens": t.outputTok,
				"cache_read_input_tokens": t.cacheRead,
				"cache_creation_input_tokens": t.cacheWrite,
			},
		})
		history = append(history, map[string]any{"role": "assistant", "content": t.assistant})

		cost, priced := table.Cost(demoModel, t.newInput, t.outputTok, t.cacheRead, t.cacheWrite, 0)
		rec := store.Record{
			StartedAt: base, DurationMS: t.durMS, TTFTMS: t.ttftMS,
			Session: Session, Provider: "anthropic", Model: demoModel,
			// Bodies are stored as plain JSON, so don't mark them streamed —
			// the Events tab would report an inconsistency.
			Method: "POST", Path: "/v1/messages", Status: 200, Streamed: false,
			InputTokens: t.newInput, OutputTokens: t.outputTok,
			CacheReadTokens: t.cacheRead, CacheWriteTokens: t.cacheWrite,
			ToolCalls: int64(len(t.tools)), ToolNames: t.tools,
			CostUSD: cost, SavedUSD: table.Saved(demoModel, t.cacheRead), Priced: priced,
			RequestBody: reqBody, ResponseBody: respBody,
		}
		if err := st.Insert(&rec); err != nil {
			return i, fmt.Errorf("insert demo record %d: %w", i, err)
		}
		base = base.Add(time.Duration(t.durMS)*time.Millisecond + 15*time.Second)
	}
	return len(turns), nil
}
