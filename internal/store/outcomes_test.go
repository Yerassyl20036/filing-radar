package store

import (
	"context"
	"math"
	"testing"
	"time"
)

func TestOutcomesAndScorecard(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	at := time.Now().Add(-48 * time.Hour)
	filings := testFilings(10, at) // even ones are 8-Ks, odd ones Form 4s
	if _, err := s.InsertFilings(ctx, filings); err != nil {
		t.Fatal(err)
	}
	score := func(n int) *int { return &n }
	label := func(i int, level string, sc *int) {
		t.Helper()
		err := s.SaveTriage(ctx, Triage{AccessionNo: filings[i].AccessionNo, Level: level, RulesVersion: 1, ModelScore: sc})
		if err != nil {
			t.Fatal(err)
		}
	}
	label(0, "high", score(85))
	label(2, "high", score(75))
	label(4, "medium", score(50))
	label(6, "low", score(10))
	label(8, "medium", nil) // not scored by the model: no outcome
	label(1, "routine", nil)

	todo, err := s.OutcomesToTrack(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	var mine []string
	for _, o := range todo {
		if o.CIK == "0000000001" && len(o.AccessionNo) > 10 && o.AccessionNo[:10] == "9999999999" {
			mine = append(mine, o.AccessionNo)
		}
	}
	// Oldest first: filing 6 was accepted earliest of the four.
	want := []string{filings[6].AccessionNo, filings[4].AccessionNo, filings[2].AccessionNo, filings[0].AccessionNo}
	if len(mine) != len(want) {
		t.Fatalf("to track: %v, want %v", mine, want)
	}
	for i := range want {
		if mine[i] != want[i] {
			t.Fatalf("to track: %v, want %v", mine, want)
		}
	}

	save := func(o Outcome) {
		t.Helper()
		if err := s.SaveOutcome(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	scored := func(i int, move float64) Outcome {
		return Outcome{AccessionNo: filings[i].AccessionNo, Status: OutcomeScored, Ticker: "TEST",
			PrevClose: 100, NextClose: 100 + move, MovePct: move, PrevDate: "2026-11-02", NextDate: "2026-11-03"}
	}
	save(Outcome{AccessionNo: filings[0].AccessionNo, Status: OutcomeNoPrice, Ticker: "TEST"})
	save(scored(0, -6)) // overwrites the "no price"
	save(scored(2, 2))
	save(scored(4, 1))
	save(Outcome{AccessionNo: filings[6].AccessionNo, Status: OutcomeNoTicker})

	todo, err = s.OutcomesToTrack(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range todo {
		if o.AccessionNo[:10] == "9999999999" {
			t.Errorf("%s still to track", o.AccessionNo)
		}
	}

	rows, err := s.Filings(ctx, FilingQuery{Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]*Outcome{}
	for _, r := range rows {
		got[r.AccessionNo] = r.Outcome
	}
	if o := got[filings[0].AccessionNo]; o == nil || o.MovePct != -6 || o.NextClose != 94 || o.PrevDate != "2026-11-02" || o.Ticker != "TEST" {
		t.Errorf("filing 0 outcome: %+v", o)
	}
	if o := got[filings[6].AccessionNo]; o != nil {
		t.Errorf("a \"no ticker\" outcome is shown: %+v", o)
	}

	// The scorecard covers every outcome in the table, so only check that
	// ours moved the numbers as expected when the table held nothing else.
	sc, err := s.Scorecard(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sc.ByScore) != 3 || len(sc.ByLevel) != 3 || sc.BigMovePct != 3 {
		t.Fatalf("shape: %+v", sc)
	}
	if sc.Scored != 3 || sc.NoTicker != 1 || sc.NoPrice != 0 {
		t.Skipf("the table holds other outcomes too (%d scored); skipping the sums", sc.Scored)
	}
	check := func(b Bucket, label string, n int, mean, share float64) {
		t.Helper()
		if b.Label != label || b.Scored != n {
			t.Errorf("bucket %+v, want %s with %d", b, label, n)
			return
		}
		if n == 0 {
			if b.MeanAbsMove != nil || b.ShareBigMove != nil {
				t.Errorf("%s: empty bucket has means", label)
			}
			return
		}
		if b.MeanAbsMove == nil || math.Abs(*b.MeanAbsMove-mean) > 1e-9 || b.ShareBigMove == nil || math.Abs(*b.ShareBigMove-share) > 1e-9 {
			t.Errorf("%s: mean %v share %v, want %v and %v", label, b.MeanAbsMove, b.ShareBigMove, mean, share)
		}
	}
	check(sc.ByScore[0], "0-39", 0, 0, 0)
	check(sc.ByScore[1], "40-69", 1, 1, 0)
	check(sc.ByScore[2], "70-100", 2, 4, 0.5)
	check(sc.ByLevel[0], "high", 2, 4, 0.5)
	check(sc.ByLevel[1], "medium", 1, 1, 0)
	check(sc.ByLevel[2], "low", 0, 0, 0)
}
