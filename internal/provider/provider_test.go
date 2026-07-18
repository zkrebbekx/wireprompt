package provider

import (
	"testing"

	. "github.com/smartystreets/goconvey/convey"
)

const anthropicJSON = `{
  "id": "msg_01",
  "type": "message",
  "model": "claude-opus-4-8",
  "content": [{"type": "text", "text": "hi"}],
  "usage": {"input_tokens": 120, "output_tokens": 34,
            "cache_creation_input_tokens": 2048, "cache_read_input_tokens": 4096}
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

			Convey("Then model and all four token buckets are extracted", func() {
				So(u.Model, ShouldEqual, "claude-opus-4-8")
				So(u.InputTokens, ShouldEqual, 120)
				So(u.OutputTokens, ShouldEqual, 34)
				So(u.CacheWriteTokens, ShouldEqual, 2048)
				So(u.CacheReadTokens, ShouldEqual, 4096)
			})
		})
	})

	Convey("Given an Anthropic SSE stream", t, func() {
		Convey("When it is reassembled", func() {
			u := Parse(FormatAnthropic, []byte(anthropicSSE), true)

			Convey("Then message_start supplies model, input and cache tokens", func() {
				So(u.Model, ShouldEqual, "claude-sonnet-5")
				So(u.InputTokens, ShouldEqual, 25)
				So(u.CacheWriteTokens, ShouldEqual, 100)
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
