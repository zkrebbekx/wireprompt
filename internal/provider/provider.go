// Package provider extracts model names and token usage from LLM API request
// and response bodies, including reassembly of server-sent-event streams.
package provider

import (
	"bufio"
	"bytes"
	"encoding/json"
	"strings"
)

// Usage is the normalized token accounting for one request.
//
// InputTokens are tokens billed at the full input rate. CacheReadTokens are
// billed at the provider's cache-read rate; CacheWriteTokens at the cache-write
// rate (Anthropic only — OpenAI has no write charge).
type Usage struct {
	Model            string
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
}

// Format identifies the wire format of a provider.
type Format string

const (
	FormatAnthropic Format = "anthropic"
	FormatOpenAI    Format = "openai"
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

// IsSSE reports whether a response content type is a server-sent-event stream.
func IsSSE(contentType string) bool {
	return strings.HasPrefix(contentType, "text/event-stream")
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
	}
	return Usage{}
}

type anthropicUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
}

func parseAnthropicJSON(body []byte) Usage {
	var resp struct {
		Model string         `json:"model"`
		Usage anthropicUsage `json:"usage"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return Usage{}
	}
	return Usage{
		Model:            resp.Model,
		InputTokens:      resp.Usage.InputTokens,
		OutputTokens:     resp.Usage.OutputTokens,
		CacheReadTokens:  resp.Usage.CacheReadInputTokens,
		CacheWriteTokens: resp.Usage.CacheCreationInputTokens,
	}
}

// parseAnthropicSSE walks the event stream: message_start carries the model,
// input and cache token counts; message_delta events carry the cumulative
// output token count.
func parseAnthropicSSE(body []byte) Usage {
	var u Usage
	forEachSSEData(body, func(data []byte) {
		var ev struct {
			Type    string `json:"type"`
			Message struct {
				Model string         `json:"model"`
				Usage anthropicUsage `json:"usage"`
			} `json:"message"`
			Usage anthropicUsage `json:"usage"`
		}
		if err := json.Unmarshal(data, &ev); err != nil {
			return
		}
		switch ev.Type {
		case "message_start":
			u.Model = ev.Message.Model
			u.InputTokens = ev.Message.Usage.InputTokens
			u.CacheReadTokens = ev.Message.Usage.CacheReadInputTokens
			u.CacheWriteTokens = ev.Message.Usage.CacheCreationInputTokens
			if ev.Message.Usage.OutputTokens > 0 {
				u.OutputTokens = ev.Message.Usage.OutputTokens
			}
		case "message_delta":
			if ev.Usage.OutputTokens > 0 {
				u.OutputTokens = ev.Usage.OutputTokens
			}
			if ev.Usage.InputTokens > 0 {
				u.InputTokens = ev.Usage.InputTokens
			}
		}
	})
	return u
}

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
}

func (u openAIUsage) normalize(model string) Usage {
	in := u.PromptTokens
	if in == 0 {
		in = u.InputTokens
	}
	out := u.CompletionTokens
	if out == 0 {
		out = u.OutputTokens
	}
	cached := u.PromptTokensDetails.CachedTokens
	if cached == 0 {
		cached = u.InputTokensDetails.CachedTokens
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
	}
}

func parseOpenAIJSON(body []byte) Usage {
	var resp struct {
		Model string      `json:"model"`
		Usage openAIUsage `json:"usage"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return Usage{}
	}
	return resp.Usage.normalize(resp.Model)
}

// parseOpenAISSE scans chat-completion chunks. The model appears on every
// chunk; usage appears on the final chunk only when the caller requested
// stream_options.include_usage — absent that, token counts stay zero.
func parseOpenAISSE(body []byte) Usage {
	var u Usage
	forEachSSEData(body, func(data []byte) {
		if bytes.Equal(data, []byte("[DONE]")) {
			return
		}
		var chunk struct {
			Model string       `json:"model"`
			Usage *openAIUsage `json:"usage"`
			// responses API stream: completed event nests the response
			Type     string `json:"type"`
			Response *struct {
				Model string      `json:"model"`
				Usage openAIUsage `json:"usage"`
			} `json:"response"`
		}
		if err := json.Unmarshal(data, &chunk); err != nil {
			return
		}
		if chunk.Response != nil && chunk.Type == "response.completed" {
			u = chunk.Response.Usage.normalize(chunk.Response.Model)
			return
		}
		if chunk.Model != "" {
			u.Model = chunk.Model
		}
		if chunk.Usage != nil {
			n := chunk.Usage.normalize(u.Model)
			u.InputTokens = n.InputTokens
			u.OutputTokens = n.OutputTokens
			u.CacheReadTokens = n.CacheReadTokens
		}
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
