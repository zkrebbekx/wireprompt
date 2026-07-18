// Package export renders captured sessions to portable formats — JSONL for
// tooling, Markdown for sharing "look what my agent did" conversations.
package export

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/zkrebbekx/wireprompt/internal/store"
)

// JSONL writes every record of a session (oldest first) as one JSON object
// per line, bodies included as strings.
func JSONL(w io.Writer, st *store.Store, session string) error {
	recs, err := st.List(store.ListOptions{Session: session, Limit: 1000})
	if err != nil {
		return err
	}
	if len(recs) == 0 {
		return fmt.Errorf("no requests in session %q", session)
	}
	enc := json.NewEncoder(w)
	for i := len(recs) - 1; i >= 0; i-- {
		full, err := st.Get(recs[i].ID)
		if err != nil {
			return err
		}
		type line struct {
			store.Record
			RequestBodyText  string `json:"request_body_text,omitempty"`
			ResponseBodyText string `json:"response_body_text,omitempty"`
		}
		l := line{Record: *full, RequestBodyText: string(full.RequestBody), ResponseBodyText: string(full.ResponseBody)}
		l.RequestBody, l.ResponseBody = nil, nil
		if err := enc.Encode(l); err != nil {
			return err
		}
	}
	return nil
}

// Markdown renders the session as a readable conversation document: a stats
// header, then the full conversation reconstructed from the last (most
// context-complete) request plus its response.
func Markdown(w io.Writer, st *store.Store, session string) error {
	recs, err := st.List(store.ListOptions{Session: session, Limit: 1000})
	if err != nil {
		return err
	}
	if len(recs) == 0 {
		return fmt.Errorf("no requests in session %q", session)
	}
	var cost, saved float64
	var in, out int64
	for _, r := range recs {
		cost += r.CostUSD
		saved += r.SavedUSD
		in += r.InputTokens + r.CacheReadTokens
		out += r.OutputTokens
	}
	last, err := st.Get(recs[0].ID) // recs is newest-first
	if err != nil {
		return err
	}

	fmt.Fprintf(w, "# wireprompt session: %s\n\n", session)
	fmt.Fprintf(w, "| requests | input tok | output tok | cost | cache saved | model |\n")
	fmt.Fprintf(w, "|---|---|---|---|---|---|\n")
	fmt.Fprintf(w, "| %d | %d | %d | $%.4f | $%.4f | %s |\n\n", len(recs), in, out, cost, saved, last.Model)

	var req map[string]json.RawMessage
	if err := json.Unmarshal(last.RequestBody, &req); err != nil {
		return fmt.Errorf("request body of #%d is not JSON (no-bodies mode?)", last.ID)
	}
	if sys, ok := req["system"]; ok {
		fmt.Fprintf(w, "## system\n\n%s\n\n", blockText(sys))
	}
	msgs := rawList(req, "messages", "input", "contents")
	for _, m := range msgs {
		var msg struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
			Parts   json.RawMessage `json:"parts"`
		}
		if err := json.Unmarshal(m, &msg); err != nil {
			continue
		}
		content := msg.Content
		if content == nil {
			content = msg.Parts
		}
		role := msg.Role
		if role == "" {
			role = "user"
		}
		fmt.Fprintf(w, "## %s\n\n%s\n\n", role, blockText(content))
	}
	// Final response.
	var resp struct {
		Content json.RawMessage `json:"content"`
		Choices []struct {
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(last.ResponseBody, &resp) == nil {
		if resp.Content != nil {
			fmt.Fprintf(w, "## assistant (final)\n\n%s\n", blockText(resp.Content))
		} else if len(resp.Choices) > 0 {
			fmt.Fprintf(w, "## assistant (final)\n\n%s\n", blockText(resp.Choices[0].Message.Content))
		}
	}
	return nil
}

func rawList(m map[string]json.RawMessage, keys ...string) []json.RawMessage {
	for _, k := range keys {
		if raw, ok := m[k]; ok {
			var list []json.RawMessage
			if json.Unmarshal(raw, &list) == nil && len(list) > 0 {
				return list
			}
		}
	}
	return nil
}

// blockText flattens a string-or-block-list content value into markdown text.
func blockText(raw json.RawMessage) string {
	if raw == nil {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []map[string]json.RawMessage
	if json.Unmarshal(raw, &blocks) != nil {
		return string(raw)
	}
	var out []string
	for _, b := range blocks {
		var typ string
		json.Unmarshal(b["type"], &typ)
		switch typ {
		case "text", "":
			var t string
			if json.Unmarshal(b["text"], &t) == nil && t != "" {
				out = append(out, t)
			}
		case "thinking":
			var t string
			json.Unmarshal(b["thinking"], &t)
			out = append(out, "> *(thinking)* "+strings.ReplaceAll(t, "\n", "\n> "))
		case "tool_use", "server_tool_use":
			var name string
			json.Unmarshal(b["name"], &name)
			out = append(out, fmt.Sprintf("**→ tool `%s`**\n\n```json\n%s\n```", name, b["input"]))
		case "tool_result":
			out = append(out, fmt.Sprintf("**← tool result**\n\n```\n%s\n```", blockText(b["content"])))
		case "image", "input_image":
			out = append(out, "*(image)*")
		default:
			out = append(out, fmt.Sprintf("```json\n%s\n```", mustJSON(b)))
		}
	}
	return strings.Join(out, "\n\n")
}

func mustJSON(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}
