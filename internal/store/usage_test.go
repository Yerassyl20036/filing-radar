package store

import (
	"context"
	"math"
	"testing"
)

func TestModelSpendToday(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Real calls may share the database, so measure the change.
	usdBefore, callsBefore, err := s.ModelSpendToday(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []ModelCall{
		{AccessionNo: "9999999999-26-000001", Purpose: "score", Provider: "deepseek", Model: "deepseek-flash", InputTokens: 3000, OutputTokens: 200, CostUSD: 0.001234},
		{AccessionNo: "9999999999-26-000001", Purpose: "draft", Provider: "anthropic", Model: "claude-test", InputTokens: 9000, OutputTokens: 800, CostUSD: 0.0405},
		{AccessionNo: "9999999999-26-000002", Purpose: "repair", CostUSD: 0.0000004}, // below numeric(12,6): rounds to 0
	} {
		if err := s.RecordModelCall(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	// A call from two days ago is outside today's budget.
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO model_calls (accession_no, purpose, cost_usd, created_at)
		VALUES ('9999999999-26-000003', 'score', 1, now() - interval '2 days')`); err != nil {
		t.Fatal(err)
	}

	usdAfter, callsAfter, err := s.ModelSpendToday(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if callsAfter-callsBefore != 3 {
		t.Errorf("calls went from %d to %d; want +3", callsBefore, callsAfter)
	}
	if d := usdAfter - usdBefore; math.Abs(d-0.041734) > 1e-9 {
		t.Errorf("spend went up by %v; want 0.041734", d)
	}

	var in, out int
	err = s.pool.QueryRow(ctx, `
		SELECT input_tokens, output_tokens FROM model_calls
		WHERE accession_no = '9999999999-26-000001' AND purpose = 'score'`).Scan(&in, &out)
	if err != nil || in != 3000 || out != 200 {
		t.Errorf("stored tokens %d/%d, %v; want 3000/200", in, out, err)
	}
}
