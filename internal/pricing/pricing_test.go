package pricing

import (
	"testing"

	. "github.com/smartystreets/goconvey/convey"
)

func TestLookup(t *testing.T) {
	Convey("Given the embedded pricing table", t, func() {
		table, err := parse(embedded)
		So(err, ShouldBeNil)

		Convey("When looking up a known Anthropic model id", func() {
			p, ok := table.Lookup("claude-opus-4-8")

			Convey("Then it resolves via prefix match with Opus prices", func() {
				So(ok, ShouldBeTrue)
				So(p.Input, ShouldEqual, 5.0)
				So(p.Output, ShouldEqual, 25.0)
			})
		})

		Convey("When looking up a dated model variant", func() {
			p, ok := table.Lookup("claude-haiku-4-5-20251001")

			Convey("Then the prefix entry still matches", func() {
				So(ok, ShouldBeTrue)
				So(p.Input, ShouldEqual, 1.0)
			})
		})

		Convey("When two prefixes could match", func() {
			p, ok := table.Lookup("gpt-4o-mini-2024-07-18")

			Convey("Then the longest prefix wins", func() {
				So(ok, ShouldBeTrue)
				So(p.Input, ShouldEqual, 0.15)
			})
		})

		Convey("When the model is unknown", func() {
			_, ok := table.Lookup("mystery-model-9000")

			Convey("Then lookup reports no match", func() {
				So(ok, ShouldBeFalse)
			})
		})
	})
}

func TestCost(t *testing.T) {
	Convey("Given the embedded pricing table", t, func() {
		table, err := parse(embedded)
		So(err, ShouldBeNil)

		Convey("When costing a Sonnet request with cache activity", func() {
			// 1M input, 1M output, 1M cache read, 1M cache write (5m)
			got, ok := table.Cost("claude-sonnet-5", 1_000_000, 1_000_000, 1_000_000, 1_000_000, 0)

			Convey("Then each bucket is billed at its own rate", func() {
				So(ok, ShouldBeTrue)
				So(got, ShouldAlmostEqual, 3.0+15.0+0.3+3.75, 1e-9)
			})
		})

		Convey("When costing with 1h cache writes", func() {
			got, ok := table.Cost("claude-sonnet-5", 0, 0, 0, 0, 1_000_000)

			Convey("Then the 1h tier bills at 2x the input rate", func() {
				So(ok, ShouldBeTrue)
				So(got, ShouldAlmostEqual, 3.75*1.6, 1e-9) // = 6.0 = 2x input
			})
		})

		Convey("When costing an unknown model", func() {
			got, ok := table.Cost("mystery-model-9000", 1000, 1000, 0, 0, 0)

			Convey("Then the cost is zero and flagged unpriced", func() {
				So(got, ShouldEqual, 0)
				So(ok, ShouldBeFalse)
			})
		})

		Convey("When looking up an OpenRouter vendor-prefixed id", func() {
			p, ok := table.Lookup("anthropic/claude-sonnet-5")

			Convey("Then the after-slash segment matches", func() {
				So(ok, ShouldBeTrue)
				So(p.Input, ShouldEqual, 3.0)
			})
		})

		Convey("When computing cache savings", func() {
			saved := table.Saved("claude-opus-4-8", 1_000_000)

			Convey("Then it is the input-rate minus cache-read-rate delta", func() {
				So(saved, ShouldAlmostEqual, 5.0-0.5, 1e-9)
			})
		})
	})
}

func TestMerge(t *testing.T) {
	Convey("Given a table and an override", t, func() {
		base, err := parse(embedded)
		So(err, ShouldBeNil)
		override := &Table{Models: []ModelPrice{
			{Match: "claude-opus-4", Input: 99, Output: 99},
			{Match: "local-llama", Input: 0, Output: 0},
		}}

		Convey("When the override is merged", func() {
			base.merge(override)

			Convey("Then same-match entries are replaced", func() {
				p, ok := base.Lookup("claude-opus-4-8")
				So(ok, ShouldBeTrue)
				So(p.Input, ShouldEqual, 99)
			})

			Convey("Then new entries are added", func() {
				_, ok := base.Lookup("local-llama-70b")
				So(ok, ShouldBeTrue)
			})
		})
	})
}
