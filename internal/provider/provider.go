// Package provider extracts model names, token usage and tool-call activity
// from LLM API request and response bodies, including reassembly of
// server-sent-event streams.
package provider

import (
	"bufio"
	"bytes"
	"encoding/json"
	"strings"
)

// Usage is the normalized accounting for one request.
//
// InputTokens are tokens billed at the full input rate. CacheReadTokens are
// billed at the provider's cache-read rate. CacheWrite5m/1h are Anthropic
// cache-creation tokens billed at 1.25x / 2x the input rate. ProviderCostUSD,
// when > 0, is an authoritative cost reported by the provider (OpenRouter)
// and preferred over table-derived pricing. ToolNames lists tools the model
// invoked in this response.
type Usage struct {
	Model            string
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWrite5m     int64
	CacheWrite1h     int64
	ProviderCostUSD  float64
	ToolNames        []string
}

// Format identifies the wire format of a provider.
type Format string

const (
	FormatAnthropic Format = "anthropic"
	FormatOpenAI    Format = "openai"
	FormatGemini    Format = "gemini"
)

// RequestModel extracts the model from a request body, used as a fallback when
// the response carries no model (e.g. error responses).
func RequestModel(body []byte) string {
	var req struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return ""
	}
	return req.Model
}

// GeminiModelFromPath extracts the model from a Gemini REST path such as
// /v1beta/models/gemini-2.5-pro:streamGenerateContent.
func GeminiModelFromPath(path string) string {
	const marker = "/models/"
	i := strings.Index(path, marker)
	if i < 0 {
		return ""
	}
	rest := path[i+len(marker):]
	if j := strings.IndexAny(rest, ":/"); j >= 0 {
		rest = rest[:j]
	}
	return rest
}

// IsSSE reports whether a response content type is a server-sent-event stream.
func IsSSE(contentType string) bool {
	return strings.HasPrefix(contentType, "text/event-stream")
}

// InjectStreamUsage rewrites an OpenAI-format streaming request body to opt
// into usage accounting (stream_options.include_usage). Returns the original
// body when the request isn't streaming, already opted in, or isn't JSON.
func InjectStreamUsage(body []byte) []byte {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}
	stream, _ := m["stream"].(bool)
	if !stream {
		return body
	}
	if _, has := m["stream_options"]; has {
		return body
	}
	m["stream_options"] = map[string]any{"include_usage": true}
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

// Parse extracts usage from a response body in the given format. sse selects
// stream reassembly vs plain JSON parsing.
func Parse(format Format, body []byte, sse bool) Usage {
	switch format {
	case FormatAnthropic:
		if sse {
			return parseAnthropicSSE(body)
		}
		return parseAnthropicJSON(body)
	case FormatOpenAI:
		if sse {
			return parseOpenAISSE(body)
		}
		return parseOpenAIJSON(body)
	case FormatGemini:
		if sse {
			return parseGeminiSSE(body)
		}
		return parseGeminiJSON(body)
	}
	return Usage{}
}

// --- Anthropic ---

type anthropicCacheCreation struct {
	Ephemeral5m int64 `json:"ephemeral_5m_input_tokens"`
	Ephemeral1h int64 `json:"ephemeral_1h_input_tokens"`
}

type anthropicUsage struct {
	InputTokens              int64                   `json:"input_tokens"`
	OutputTokens             int64                   `json:"output_tokens"`
	CacheCreationInputTokens int64                   `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64                   `json:"cache_read_input_tokens"`
	CacheCreation            *anthropicCacheCreation `json:"cache_creation"`
}

func (a anthropicUsage) apply(u *Usage) {
	if a.InputTokens > 0 {
		u.InputTokens = a.InputTokens
	}
	if a.OutputTokens > 0 {
		u.OutputTokens = a.OutputTokens
	}
	if a.CacheReadInputTokens > 0 {
		u.CacheReadTokens = a.CacheReadInputTokens
	}
	if a.CacheCreation != nil && (a.CacheCreation.Ephemeral5m > 0 || a.CacheCreation.Ephemeral1h > 0) {
		u.CacheWrite5m = a.CacheCreation.Ephemeral5m
		u.CacheWrite1h = a.CacheCreation.Ephemeral1h
	} else if a.CacheCreationInputTokens > 0 && u.CacheWrite5m == 0 && u.CacheWrite1h == 0 {
		// Older responses report only the unsplit total; bill it at the
		// cheaper 5m tier. Never overwrite a split already recorded — SSE
		// message_delta events repeat the legacy total after message_start
		// delivered the 5m/1h breakdown, which would double-count.
		u.CacheWrite5m = a.CacheCreationInputTokens
	}
}

type anthropicContentBlock struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

func parseAnthropicJSON(body []byte) Usage {
	var resp struct {
		Model   string                  `json:"model"`
		Usage   anthropicUsage          `json:"usage"`
		Content []anthropicContentBlock `json:"content"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return Usage{}
	}
	u := Usage{Model: resp.Model}
	resp.Usage.apply(&u)
	for _, b := range resp.Content {
		if b.Type == "tool_use" || b.Type == "server_tool_use" {
			u.ToolNames = append(u.ToolNames, b.Name)
		}
	}
	return u
}

// parseAnthropicSSE walks the event stream: message_start carries the model,
// input and cache token counts; message_delta events carry the cumulative
// output token count; content_block_start events reveal tool invocations.
func parseAnthropicSSE(body []byte) Usage {
	var u Usage
	forEachSSEData(body, func(data []byte) {
		var ev struct {
			Type    string `json:"type"`
			Message struct {
				Model string         `json:"model"`
				Usage anthropicUsage `json:"usage"`
			} `json:"message"`
			Usage        anthropicUsage        `json:"usage"`
			ContentBlock anthropicContentBlock `json:"content_block"`
		}
		if err := json.Unmarshal(data, &ev); err != nil {
			return
		}
		switch ev.Type {
		case "message_start":
			u.Model = ev.Message.Model
			ev.Message.Usage.apply(&u)
		case "message_delta":
			ev.Usage.apply(&u)
		case "content_block_start":
			if ev.ContentBlock.Type == "tool_use" || ev.ContentBlock.Type == "server_tool_use" {
				u.ToolNames = append(u.ToolNames, ev.ContentBlock.Name)
			}
		}
	})
	return u
}

// --- OpenAI (and compatible: OpenRouter, Ollama, vLLM) ---

type openAIUsage struct {
	PromptTokens        int64 `json:"prompt_tokens"`
	CompletionTokens    int64 `json:"completion_tokens"`
	InputTokens         int64 `json:"input_tokens"`  // responses API
	OutputTokens        int64 `json:"output_tokens"` // responses API
	PromptTokensDetails struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	InputTokensDetails struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	// OpenRouter reports authoritative cost in credits (USD) when usage
	// accounting is requested.
	Cost float64 `json:"cost"`
}

func (o openAIUsage) normalize(model string) Usage {
	in := o.PromptTokens
	if in == 0 {
		in = o.InputTokens
	}
	out := o.CompletionTokens
	if out == 0 {
		out = o.OutputTokens
	}
	cached := o.PromptTokensDetails.CachedTokens
	if cached == 0 {
		cached = o.InputTokensDetails.CachedTokens
	}
	// OpenAI's prompt_tokens includes cached tokens; split them out so each
	// bucket is billed at its own rate.
	if cached > in {
		cached = in
	}
	return Usage{
		Model:           model,
		InputTokens:     in - cached,
		OutputTokens:    out,
		CacheReadTokens: cached,
		ProviderCostUSD: o.Cost,
	}
}

type openAIToolCall struct {
	Function struct {
		Name string `json:"name"`
	} `json:"function"`
}

func parseOpenAIJSON(body []byte) Usage {
	var resp struct {
		Model   string      `json:"model"`
		Usage   openAIUsage `json:"usage"`
		Choices []struct {
			Message struct {
				ToolCalls []openAIToolCall `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		// responses API
		Output []struct {
			Type string `json:"type"`
			Name string `json:"name"`
		} `json:"output"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return Usage{}
	}
	u := resp.Usage.normalize(resp.Model)
	for _, c := range resp.Choices {
		for _, tc := range c.Message.ToolCalls {
			if tc.Function.Name != "" {
				u.ToolNames = append(u.ToolNames, tc.Function.Name)
			}
		}
	}
	for _, o := range resp.Output {
		if o.Type == "function_call" && o.Name != "" {
			u.ToolNames = append(u.ToolNames, o.Name)
		}
	}
	return u
}

// parseOpenAISSE scans chat-completion chunks. The model appears on every
// chunk; usage appears on the final chunk only when the caller requested
// stream_options.include_usage (the proxy injects this by default).
func parseOpenAISSE(body []byte) Usage {
	var u Usage
	seenTools := map[string]bool{}
	forEachSSEData(body, func(data []byte) {
		if bytes.Equal(data, []byte("[DONE]")) {
			return
		}
		var chunk struct {
			Model   string       `json:"model"`
			Usage   *openAIUsage `json:"usage"`
			Choices []struct {
				Delta struct {
					ToolCalls []openAIToolCall `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
			// responses API stream: completed event nests the response
			Type     string `json:"type"`
			Response *struct {
				Model string      `json:"model"`
				Usage openAIUsage `json:"usage"`
			} `json:"response"`
			Item *struct {
				Type string `json:"type"`
				Name string `json:"name"`
			} `json:"item"`
		}
		if err := json.Unmarshal(data, &chunk); err != nil {
			return
		}
		if chunk.Response != nil && (chunk.Type == "response.completed" ||
			chunk.Type == "response.incomplete" || chunk.Type == "response.failed") {
			tools := u.ToolNames
			u = chunk.Response.Usage.normalize(chunk.Response.Model)
			u.ToolNames = tools
			return
		}
		if chunk.Item != nil && chunk.Type == "response.output_item.added" &&
			chunk.Item.Type == "function_call" && chunk.Item.Name != "" && !seenTools[chunk.Item.Name] {
			u.ToolNames = append(u.ToolNames, chunk.Item.Name)
		}
		if chunk.Model != "" {
			u.Model = chunk.Model
		}
		for _, c := range chunk.Choices {
			for _, tc := range c.Delta.ToolCalls {
				if tc.Function.Name != "" {
					u.ToolNames = append(u.ToolNames, tc.Function.Name)
				}
			}
		}
		if chunk.Usage != nil {
			n := chunk.Usage.normalize(u.Model)
			u.InputTokens = n.InputTokens
			u.OutputTokens = n.OutputTokens
			u.CacheReadTokens = n.CacheReadTokens
			u.ProviderCostUSD = n.ProviderCostUSD
		}
	})
	return u
}

// --- Gemini ---

type geminiUsage struct {
	PromptTokenCount        int64 `json:"promptTokenCount"`
	CandidatesTokenCount    int64 `json:"candidatesTokenCount"`
	CachedContentTokenCount int64 `json:"cachedContentTokenCount"`
	ThoughtsTokenCount      int64 `json:"thoughtsTokenCount"`
}

func (g geminiUsage) apply(u *Usage) {
	if g.PromptTokenCount > 0 {
		in := g.PromptTokenCount - g.CachedContentTokenCount
		if in < 0 {
			in = 0
		}
		u.InputTokens = in
		u.CacheReadTokens = g.CachedContentTokenCount
	}
	if out := g.CandidatesTokenCount + g.ThoughtsTokenCount; out > 0 {
		u.OutputTokens = out
	}
}

type geminiChunk struct {
	ModelVersion  string      `json:"modelVersion"`
	UsageMetadata geminiUsage `json:"usageMetadata"`
	Candidates    []struct {
		Content struct {
			Parts []struct {
				FunctionCall *struct {
					Name string `json:"name"`
				} `json:"functionCall"`
			} `json:"parts"`
		} `json:"content"`
	} `json:"candidates"`
}

func (c geminiChunk) apply(u *Usage) {
	if c.ModelVersion != "" {
		u.Model = c.ModelVersion
	}
	c.UsageMetadata.apply(u)
	for _, cand := range c.Candidates {
		for _, p := range cand.Content.Parts {
			if p.FunctionCall != nil && p.FunctionCall.Name != "" {
				u.ToolNames = append(u.ToolNames, p.FunctionCall.Name)
			}
		}
	}
}

func parseGeminiJSON(body []byte) Usage {
	var resp geminiChunk
	if err := json.Unmarshal(body, &resp); err != nil {
		return Usage{}
	}
	var u Usage
	resp.apply(&u)
	return u
}

func parseGeminiSSE(body []byte) Usage {
	var u Usage
	forEachSSEData(body, func(data []byte) {
		var chunk geminiChunk
		if err := json.Unmarshal(data, &chunk); err != nil {
			return
		}
		chunk.apply(&u)
	})
	return u
}

// forEachSSEData invokes fn with the payload of every `data:` line. Multi-line
// data fields are passed line by line, which is fine for LLM APIs — they emit
// single-line JSON payloads.
func forEachSSEData(body []byte, fn func(data []byte)) {
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if rest, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			fn(bytes.TrimSpace(rest))
		}
	}
}
