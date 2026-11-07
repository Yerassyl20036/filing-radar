package store

import (
	"context"
	"math"
	"testing"
	"time"
)

func TestModelCosts(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Real calls may share the database, so measure the change.
	before, err := s.ModelCosts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []ModelCall{
		{AccessionNo: "9999999999-26-000101", Purpose: "score", InputTokens: 3000, OutputTokens: 200, CostUSD: 0.001},
		{AccessionNo: "9999999999-26-000101", Purpose: "draft", InputTokens: 9000, OutputTokens: 800, CostUSD: 0.04},
	} {
		if err := s.RecordModelCall(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	// A call from two days ago counts all time, not today.
	old := time.Now().Add(-48 * time.Hour)
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO model_calls (accession_no, purpose, input_tokens, output_tokens, cost_usd, created_at)
		VALUES ('9999999999-26-000102', 'repair', 500, 50, 0.5, $1)`, old); err != nil {
		t.Fatal(err)
	}

	after, err := s.ModelCosts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	check := func(name string, b, a Spend, calls int, in, out int64, usd float64) {
		t.Helper()
		if a.Calls-b.Calls != calls || a.InputTokens-b.InputTokens != in || a.OutputTokens-b.OutputTokens != out {
			t.Errorf("%s: calls/in/out went up by %d/%d/%d; want %d/%d/%d", name,
				a.Calls-b.Calls, a.InputTokens-b.InputTokens, a.OutputTokens-b.OutputTokens, calls, in, out)
		}
		if d := a.USD - b.USD; math.Abs(d-usd) > 1e-9 {
			t.Errorf("%s: spend went up by %v; want %v", name, d, usd)
		}
		if a.Since == nil {
			t.Errorf("%s: since is nil after calls were recorded", name)
		}
	}
	check("all time", before.AllTime, after.AllTime, 3, 12500, 1050, 0.541)
	check("today", before.Today, after.Today, 2, 12000, 1000, 0.041)
	if after.AllTime.Since != nil && after.AllTime.Since.After(old.Add(time.Second)) {
		t.Errorf("all-time since is %v; want no later than %v", after.AllTime.Since, old)
	}

	byPurpose := func(c Costs) map[string]PurposeSpend {
		m := map[string]PurposeSpend{}
		for _, p := range c.ByPurpose {
			m[p.Purpose] = p
		}
		return m
	}
	b, a := byPurpose(before), byPurpose(after)
	for purpose, usd := range map[string]float64{"score": 0.001, "draft": 0.04, "repair": 0.5} {
		if a[purpose].Calls-b[purpose].Calls != 1 || math.Abs(a[purpose].USD-b[purpose].USD-usd) > 1e-9 {
			t.Errorf("%s: went from %+v to %+v; want +1 call, +%v", purpose, b[purpose], a[purpose], usd)
		}
	}
}
