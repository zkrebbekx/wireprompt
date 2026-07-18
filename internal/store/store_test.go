package store

import (
	"path/filepath"
	"testing"
	"time"

	. "github.com/smartystreets/goconvey/convey"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func sample(session, model string, cost float64) *Record {
	return &Record{
		StartedAt: time.Now(), DurationMS: 1200, TTFTMS: 300,
		Session: session, Provider: "anthropic", Model: model,
		Method: "POST", Path: "/v1/messages", Status: 200, Streamed: true,
		InputTokens: 100, OutputTokens: 50, CacheReadTokens: 10, CacheWriteTokens: 5,
		CostUSD: cost, RequestBody: []byte(`{"model":"x"}`), ResponseBody: []byte(`{"ok":true}`),
	}
}

func TestInsertAndGet(t *testing.T) {
	Convey("Given an open store", t, func() {
		st := openTestStore(t)

		Convey("When a record is inserted", func() {
			r := sample("sess-a", "claude-opus-4-8", 0.0123)
			So(st.Insert(r), ShouldBeNil)

			Convey("Then it gets an id", func() {
				So(r.ID, ShouldBeGreaterThan, 0)
			})

			Convey("Then Get returns it with bodies intact", func() {
				got, err := st.Get(r.ID)
				So(err, ShouldBeNil)
				So(got.Session, ShouldEqual, "sess-a")
				So(got.Model, ShouldEqual, "claude-opus-4-8")
				So(got.Streamed, ShouldBeTrue)
				So(string(got.RequestBody), ShouldEqual, `{"model":"x"}`)
				So(string(got.ResponseBody), ShouldEqual, `{"ok":true}`)
				So(got.CostUSD, ShouldAlmostEqual, 0.0123, 1e-9)
			})
		})
	})
}

func TestList(t *testing.T) {
	Convey("Given a store with records from two sessions", t, func() {
		st := openTestStore(t)
		So(st.Insert(sample("sess-a", "claude-opus-4-8", 0.01)), ShouldBeNil)
		So(st.Insert(sample("sess-b", "gpt-4o", 0.02)), ShouldBeNil)
		So(st.Insert(sample("sess-a", "claude-haiku-4-5", 0.03)), ShouldBeNil)

		Convey("When listing everything", func() {
			recs, err := st.List(ListOptions{})
			So(err, ShouldBeNil)

			Convey("Then all records return newest-first without bodies", func() {
				So(len(recs), ShouldEqual, 3)
				So(recs[0].Model, ShouldEqual, "claude-haiku-4-5")
				So(recs[0].RequestBody, ShouldBeNil)
			})
		})

		Convey("When filtering by session", func() {
			recs, err := st.List(ListOptions{Session: "sess-a"})
			So(err, ShouldBeNil)

			Convey("Then only that session's records return", func() {
				So(len(recs), ShouldEqual, 2)
			})
		})

		Convey("When filtering by model prefix", func() {
			recs, err := st.List(ListOptions{Model: "claude"})
			So(err, ShouldBeNil)

			Convey("Then only matching models return", func() {
				So(len(recs), ShouldEqual, 2)
			})
		})
	})
}

func TestStats(t *testing.T) {
	Convey("Given a store with records across models", t, func() {
		st := openTestStore(t)
		So(st.Insert(sample("sess-a", "claude-opus-4-8", 0.10)), ShouldBeNil)
		So(st.Insert(sample("sess-a", "claude-opus-4-8", 0.20)), ShouldBeNil)
		So(st.Insert(sample("sess-b", "gpt-4o", 0.05)), ShouldBeNil)

		Convey("When aggregating by model", func() {
			rows, err := st.Stats("model", time.Time{})
			So(err, ShouldBeNil)

			Convey("Then rows are grouped and ordered by cost", func() {
				So(len(rows), ShouldEqual, 2)
				So(rows[0].Key, ShouldEqual, "claude-opus-4-8")
				So(rows[0].Requests, ShouldEqual, 2)
				So(rows[0].CostUSD, ShouldAlmostEqual, 0.30, 1e-9)
				So(rows[0].InputTokens, ShouldEqual, 200)
			})
		})

		Convey("When aggregating by session since the future", func() {
			rows, err := st.Stats("session", time.Now().Add(time.Hour))
			So(err, ShouldBeNil)

			Convey("Then nothing matches", func() {
				So(len(rows), ShouldEqual, 0)
			})
		})
	})
}
