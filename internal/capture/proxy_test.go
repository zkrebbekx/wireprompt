package capture

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/zkrebbekx/wireprompt/internal/pricing"
	"github.com/zkrebbekx/wireprompt/internal/provider"
	"github.com/zkrebbekx/wireprompt/internal/store"
)

const upstreamJSON = `{"id":"msg_01","model":"claude-opus-4-8","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":100,"output_tokens":25,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}`

const upstreamSSE = "event: message_start\n" +
	`data: {"type":"message_start","message":{"model":"claude-haiku-4-5","usage":{"input_tokens":10,"output_tokens":1}}}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"Hello"}}` + "\n\n" +
	"event: message_delta\n" +
	`data: {"type":"message_delta","usage":{"output_tokens":9}}` + "\n\n"

func newTestProxy(t *testing.T, upstream *httptest.Server) (*Proxy, *store.Store, *[]store.Record) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	table, err := pricing.Load()
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(upstream.URL)
	notified := &[]store.Record{}
	p := New(st, table, []Route{{Name: "anthropic", Upstream: u, Format: provider.FormatAnthropic}},
		func(r store.Record) { *notified = append(*notified, r) })
	return p, st, notified
}

// eventually polls cond until true or the deadline passes. Capture completes
// asynchronously relative to the client: with a Content-Length response the
// client can finish reading before the proxy has inserted the record.
func eventually(cond func() bool) bool {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return cond()
}

func TestProxyJSONCapture(t *testing.T) {
	Convey("Given an upstream returning a JSON message response", t, func() {
		var gotPath, gotAuth string
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
			gotAuth = r.Header.Get("x-api-key")
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(upstreamJSON))
		}))
		defer upstream.Close()
		p, st, notified := newTestProxy(t, upstream)
		front := httptest.NewServer(p)
		defer front.Close()

		Convey("When a client posts through the session-scoped proxy path", func() {
			req, _ := http.NewRequest("POST", front.URL+"/s/mysession/anthropic/v1/messages",
				strings.NewReader(`{"model":"claude-opus-4-8","messages":[]}`))
			req.Header.Set("x-api-key", "sk-test")
			resp, err := http.DefaultClient.Do(req)
			So(err, ShouldBeNil)
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()

			Convey("Then the response passes through unmodified", func() {
				So(resp.StatusCode, ShouldEqual, 200)
				So(string(body), ShouldEqual, upstreamJSON)
			})

			Convey("Then the upstream saw the stripped path and auth header", func() {
				So(gotPath, ShouldEqual, "/v1/messages")
				So(gotAuth, ShouldEqual, "sk-test")
			})

			Convey("Then a record lands in the store with usage and cost", func() {
				So(eventually(func() bool {
					recs, _ := st.List(store.ListOptions{})
					return len(recs) == 1
				}), ShouldBeTrue)
				recs, err := st.List(store.ListOptions{})
				So(err, ShouldBeNil)
				So(len(recs), ShouldEqual, 1)
				r := recs[0]
				So(r.Session, ShouldEqual, "mysession")
				So(r.Provider, ShouldEqual, "anthropic")
				So(r.Model, ShouldEqual, "claude-opus-4-8")
				So(r.InputTokens, ShouldEqual, 100)
				So(r.OutputTokens, ShouldEqual, 25)
				// 100 in @ $5/M + 25 out @ $25/M
				So(r.CostUSD, ShouldAlmostEqual, 100.0/1e6*5+25.0/1e6*25, 1e-12)
				So(r.TTFTMS, ShouldBeGreaterThanOrEqualTo, 0)
			})

			Convey("Then the live feed was notified without bodies", func() {
				So(eventually(func() bool { return len(*notified) == 1 }), ShouldBeTrue)
				So((*notified)[0].RequestBody, ShouldBeNil)
			})
		})
	})
}

func TestProxySSECapture(t *testing.T) {
	Convey("Given an upstream that streams SSE", t, func() {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			flusher := w.(http.Flusher)
			for _, chunk := range strings.SplitAfter(upstreamSSE, "\n\n") {
				w.Write([]byte(chunk))
				flusher.Flush()
				time.Sleep(5 * time.Millisecond)
			}
		}))
		defer upstream.Close()
		p, st, _ := newTestProxy(t, upstream)
		front := httptest.NewServer(p)
		defer front.Close()

		Convey("When a client posts through the default-session path", func() {
			resp, err := http.Post(front.URL+"/anthropic/v1/messages", "application/json",
				strings.NewReader(`{"model":"claude-haiku-4-5","stream":true,"messages":[]}`))
			So(err, ShouldBeNil)
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()

			Convey("Then the full stream reaches the client", func() {
				So(string(body), ShouldEqual, upstreamSSE)
			})

			Convey("Then the record marks the request streamed with reassembled usage", func() {
				recs, err := st.List(store.ListOptions{})
				So(err, ShouldBeNil)
				So(len(recs), ShouldEqual, 1)
				r := recs[0]
				So(r.Session, ShouldEqual, "default")
				So(r.Streamed, ShouldBeTrue)
				So(r.Model, ShouldEqual, "claude-haiku-4-5")
				So(r.InputTokens, ShouldEqual, 10)
				So(r.OutputTokens, ShouldEqual, 9)
			})
		})
	})
}

func TestSplitPath(t *testing.T) {
	Convey("Given proxy path variants", t, func() {
		Convey("When parsing a session-scoped path", func() {
			session, route, rest, ok := splitPath("/s/abc/anthropic/v1/messages")

			Convey("Then all segments are extracted", func() {
				So(ok, ShouldBeTrue)
				So(session, ShouldEqual, "abc")
				So(route, ShouldEqual, "anthropic")
				So(rest, ShouldEqual, "/v1/messages")
			})
		})

		Convey("When parsing a bare provider path", func() {
			session, route, rest, ok := splitPath("/openai/v1/chat/completions")

			Convey("Then the session defaults", func() {
				So(ok, ShouldBeTrue)
				So(session, ShouldEqual, "default")
				So(route, ShouldEqual, "openai")
				So(rest, ShouldEqual, "/v1/chat/completions")
			})
		})

		Convey("When parsing a too-short path", func() {
			_, _, _, ok := splitPath("/nothing")

			Convey("Then it does not match", func() {
				So(ok, ShouldBeFalse)
			})
		})
	})
}
