package provider

import (
	"bytes"
	"encoding/json"
	"testing"

	. "github.com/smartystreets/goconvey/convey"
)

const anthropicJSON = `{
  "id": "msg_01",
  "type": "message",
  "model": "claude-opus-4-8",
  "content": [{"type": "text", "text": "hi"}, {"type": "tool_use", "name": "Read", "input": {}}],
  "usage": {"input_tokens": 120, "output_tokens": 34,
            "cache_creation_input_tokens": 2048, "cache_read_input_tokens": 4096,
            "cache_creation": {"ephemeral_5m_input_tokens": 1500, "ephemeral_1h_input_tokens": 548}}
}`

const anthropicSSE = "event: message_start\n" +
	`data: {"type":"message_start","message":{"id":"msg_01","model":"claude-sonnet-5","usage":{"input_tokens":25,"output_tokens":1,"cache_creation_input_tokens":100,"cache_read_input_tokens":200}}}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}` + "\n\n" +
	"event: message_delta\n" +
	`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":57}}` + "\n\n" +
	"event: message_stop\n" +
	`data: {"type":"message_stop"}` + "\n\n"

const openAIJSON = `{
  "id": "chatcmpl-1", "model": "gpt-4o-2024-08-06",
  "choices": [{"message": {"role": "assistant", "content": "hi"}}],
  "usage": {"prompt_tokens": 1000, "completion_tokens": 50,
            "prompt_tokens_details": {"cached_tokens": 700}}
}`

const openAISSE = `data: {"id":"chatcmpl-1","model":"gpt-4o-mini","choices":[{"delta":{"content":"He"}}]}` + "\n\n" +
	`data: {"id":"chatcmpl-1","model":"gpt-4o-mini","choices":[{"delta":{"content":"llo"}}]}` + "\n\n" +
	`data: {"id":"chatcmpl-1","model":"gpt-4o-mini","choices":[],"usage":{"prompt_tokens":80,"completion_tokens":12,"prompt_tokens_details":{"cached_tokens":30}}}` + "\n\n" +
	"data: [DONE]\n\n"

func TestAnthropicParsing(t *testing.T) {
	Convey("Given a non-streaming Anthropic response body", t, func() {
		Convey("When it is parsed", func() {
			u := Parse(FormatAnthropic, []byte(anthropicJSON), false)

			Convey("Then model and all token buckets are extracted", func() {
				So(u.Model, ShouldEqual, "claude-opus-4-8")
				So(u.InputTokens, ShouldEqual, 120)
				So(u.OutputTokens, ShouldEqual, 34)
				So(u.CacheReadTokens, ShouldEqual, 4096)
			})

			Convey("Then the cache_creation split takes precedence over the unsplit total", func() {
				So(u.CacheWrite5m, ShouldEqual, 1500)
				So(u.CacheWrite1h, ShouldEqual, 548)
			})

			Convey("Then tool invocations are extracted", func() {
				So(u.ToolNames, ShouldResemble, []string{"Read"})
			})
		})
	})

	Convey("Given an Anthropic SSE stream", t, func() {
		Convey("When it is reassembled", func() {
			u := Parse(FormatAnthropic, []byte(anthropicSSE), true)

			Convey("Then message_start supplies model, input and cache tokens", func() {
				So(u.Model, ShouldEqual, "claude-sonnet-5")
				So(u.InputTokens, ShouldEqual, 25)
				So(u.CacheWrite5m, ShouldEqual, 100)
				So(u.CacheReadTokens, ShouldEqual, 200)
			})

			Convey("Then message_delta supplies the final output count", func() {
				So(u.OutputTokens, ShouldEqual, 57)
			})
		})
	})
}

func TestOpenAIParsing(t *testing.T) {
	Convey("Given a non-streaming OpenAI response body", t, func() {
		Convey("When it is parsed", func() {
			u := Parse(FormatOpenAI, []byte(openAIJSON), false)

			Convey("Then cached tokens are split out of prompt tokens", func() {
				So(u.Model, ShouldEqual, "gpt-4o-2024-08-06")
				So(u.InputTokens, ShouldEqual, 300) // 1000 - 700 cached
				So(u.CacheReadTokens, ShouldEqual, 700)
				So(u.OutputTokens, ShouldEqual, 50)
			})
		})
	})

	Convey("Given an OpenAI SSE stream with include_usage", t, func() {
		Convey("When it is reassembled", func() {
			u := Parse(FormatOpenAI, []byte(openAISSE), true)

			Convey("Then the final usage chunk wins", func() {
				So(u.Model, ShouldEqual, "gpt-4o-mini")
				So(u.InputTokens, ShouldEqual, 50) // 80 - 30 cached
				So(u.CacheReadTokens, ShouldEqual, 30)
				So(u.OutputTokens, ShouldEqual, 12)
			})
		})
	})
}

const geminiJSON = `{
  "modelVersion": "gemini-2.5-pro",
  "candidates": [{"content": {"parts": [
    {"text": "Sure."},
    {"functionCall": {"name": "search_files", "args": {}}}
  ]}}],
  "usageMetadata": {"promptTokenCount": 500, "candidatesTokenCount": 40,
                    "cachedContentTokenCount": 300, "thoughtsTokenCount": 25}
}`

func TestGeminiParsing(t *testing.T) {
	Convey("Given a Gemini generateContent response", t, func() {
		Convey("When it is parsed", func() {
			u := Parse(FormatGemini, []byte(geminiJSON), false)

			Convey("Then cached content is split out and thoughts count as output", func() {
				So(u.Model, ShouldEqual, "gemini-2.5-pro")
				So(u.InputTokens, ShouldEqual, 200) // 500 - 300 cached
				So(u.CacheReadTokens, ShouldEqual, 300)
				So(u.OutputTokens, ShouldEqual, 65) // 40 + 25 thoughts
			})

			Convey("Then function calls are extracted as tools", func() {
				So(u.ToolNames, ShouldResemble, []string{"search_files"})
			})
		})
	})

	Convey("Given a Gemini streaming body", t, func() {
		// SSE data payloads are single-line JSON on the wire.
		var compact bytes.Buffer
		So(json.Compact(&compact, []byte(geminiJSON)), ShouldBeNil)
		sse := "data: " + compact.String() + "\n\n"

		Convey("When it is reassembled", func() {
			u := Parse(FormatGemini, []byte(sse), true)

			Convey("Then usage matches the JSON parse", func() {
				So(u.InputTokens, ShouldEqual, 200)
				So(u.OutputTokens, ShouldEqual, 65)
			})
		})
	})

	Convey("Given a Gemini REST path", t, func() {
		Convey("When the model is extracted from it", func() {
			m := GeminiModelFromPath("/v1beta/models/gemini-2.5-flash:streamGenerateContent")

			Convey("Then the segment between /models/ and the colon returns", func() {
				So(m, ShouldEqual, "gemini-2.5-flash")
			})
		})
	})
}

func TestInjectStreamUsage(t *testing.T) {
	Convey("Given a streaming OpenAI request without stream_options", t, func() {
		body := []byte(`{"model":"gpt-4o","stream":true,"messages":[]}`)

		Convey("When usage injection runs", func() {
			out := InjectStreamUsage(body)

			Convey("Then include_usage is added", func() {
				So(string(out), ShouldContainSubstring, `"include_usage":true`)
			})
		})
	})

	Convey("Given a non-streaming request", t, func() {
		body := []byte(`{"model":"gpt-4o","messages":[]}`)

		Convey("When usage injection runs", func() {
			out := InjectStreamUsage(body)

			Convey("Then the body is untouched", func() {
				So(string(out), ShouldEqual, string(body))
			})
		})
	})

	Convey("Given a request that already sets stream_options", t, func() {
		body := []byte(`{"model":"gpt-4o","stream":true,"stream_options":{"include_usage":false}}`)

		Convey("When usage injection runs", func() {
			out := InjectStreamUsage(body)

			Convey("Then the caller's choice is respected", func() {
				So(string(out), ShouldEqual, string(body))
			})
		})
	})
}

func TestOpenRouterCost(t *testing.T) {
	Convey("Given an OpenRouter response with provider-reported cost", t, func() {
		body := []byte(`{"model":"anthropic/claude-sonnet-5",
			"choices":[{"message":{"content":"hi"}}],
			"usage":{"prompt_tokens":100,"completion_tokens":10,"cost":0.0123}}`)

		Convey("When it is parsed", func() {
			u := Parse(FormatOpenAI, []byte(body), false)

			Convey("Then the authoritative cost is surfaced", func() {
				So(u.ProviderCostUSD, ShouldAlmostEqual, 0.0123, 1e-9)
			})
		})
	})
}

func TestRequestModel(t *testing.T) {
	Convey("Given a request body with a model field", t, func() {
		body := []byte(`{"model": "claude-haiku-4-5", "messages": []}`)

		Convey("When the model is extracted", func() {
			m := RequestModel(body)

			Convey("Then it matches the request", func() {
				So(m, ShouldEqual, "claude-haiku-4-5")
			})
		})
	})

	Convey("Given a malformed body", t, func() {
		Convey("When the model is extracted", func() {
			m := RequestModel([]byte("not json"))

			Convey("Then the result is empty", func() {
				So(m, ShouldBeEmpty)
			})
		})
	})
}
