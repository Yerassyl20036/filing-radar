package store

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Yerassyl20036/filing-radar/internal/edgar"
)

// Real filings can share the test database, so assertions look only at test
// rows: far-future accepted_at puts them first in the feed, and a 1900
// first_seen_at puts them first in Undecided.
var (
	farFuture = time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)
	longAgo   = time.Date(1900, 1, 1, 0, 0, 0, 0, time.UTC)
)

func isTestRow(accessionNo string) bool { return strings.HasPrefix(accessionNo, "9999999999-") }

// seedFilings stores n test filings (see testFilings) seen in 1900.
func seedFilings(t *testing.T, s *Store, n int) []edgar.Filing {
	t.Helper()
	ctx := context.Background()
	filings := testFilings(n, farFuture)
	if _, err := s.InsertFilings(ctx, filings); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE filings SET first_seen_at = $1 WHERE accession_no LIKE '9999999999-%'`, longAgo); err != nil {
		t.Fatal(err)
	}
	return filings
}

func saveTriage(t *testing.T, s *Store, tr Triage) {
	t.Helper()
	if err := s.SaveTriage(context.Background(), tr); err != nil {
		t.Fatal(err)
	}
}

func testAccessions(rows []FilingRow) []string {
	var out []string
	for _, r := range rows {
		if isTestRow(r.AccessionNo) {
			out = append(out, r.AccessionNo)
		}
	}
	return out
}

func TestDetailsRoundTrip(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	acc := seedFilings(t, s, 1)[0].AccessionNo

	// pgx sends nil as NULL; the column is NOT NULL, so this must become '{}'.
	if err := s.SaveDetails(ctx, acc, nil, ""); err != nil {
		t.Fatalf("save nil items: %v", err)
	}
	items, doc, found, err := s.Details(ctx, acc)
	if err != nil || !found || items == nil || len(items) != 0 || doc != "" {
		t.Fatalf("after nil items: %q, %q, %v, %v; want [], \"\", true, nil", items, doc, found, err)
	}

	if err := s.SaveDetails(ctx, acc, []string{"2.02", "9.01"}, "ex99-1.htm"); err != nil {
		t.Fatalf("save again: %v", err)
	}
	items, doc, found, err = s.Details(ctx, acc)
	if err != nil || !found || !slices.Equal(items, []string{"2.02", "9.01"}) || doc != "ex99-1.htm" {
		t.Fatalf("after overwrite: %q, %q, %v, %v", items, doc, found, err)
	}

	items, doc, found, err = s.Details(ctx, "9999999999-26-999999")
	if err != nil || found || items != nil || doc != "" {
		t.Fatalf("missing row: %q, %q, %v, %v; want nil, \"\", false, nil", items, doc, found, err)
	}
}

func TestSaveTriageOverwrites(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	acc := seedFilings(t, s, 1)[0].AccessionNo

	// Nil reasons, like nil items, must not reach the NOT NULL column as NULL.
	saveTriage(t, s, Triage{AccessionNo: acc, Level: "low", RulesVersion: 1})
	var firstDecided time.Time
	if err := s.pool.QueryRow(ctx, `SELECT decided_at FROM triage WHERE accession_no = $1`, acc).Scan(&firstDecided); err != nil {
		t.Fatal(err)
	}

	score := 80
	saveTriage(t, s, Triage{
		AccessionNo:  acc,
		Level:        "high",
		Reasons:      []string{"item 1.03", "large filer"},
		Items:        []string{"ignored: items live in filing_details"},
		RulesVersion: 2,
		ModelScore:   &score,
		ModelReason:  "bankruptcy",
		Model:        "test-model",
	})

	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM triage WHERE accession_no = $1`, acc).Scan(&n); err != nil || n != 1 {
		t.Fatalf("triage rows for %s: %d, %v; want 1", acc, n, err)
	}
	var got Triage
	err := s.pool.QueryRow(ctx, `
		SELECT level, reasons, rules_version, model_score, model_reason, model, decided_at
		FROM triage WHERE accession_no = $1`, acc).
		Scan(&got.Level, &got.Reasons, &got.RulesVersion, &got.ModelScore, &got.ModelReason, &got.Model, &got.DecidedAt)
	if err != nil {
		t.Fatal(err)
	}
	if got.Level != "high" || !slices.Equal(got.Reasons, []string{"item 1.03", "large filer"}) || got.RulesVersion != 2 ||
		got.ModelScore == nil || *got.ModelScore != 80 || got.ModelReason != "bankruptcy" || got.Model != "test-model" {
		t.Errorf("after overwrite: %+v (model_score %v)", got, got.ModelScore)
	}
	if got.DecidedAt.Before(firstDecided) {
		t.Errorf("decided_at went back: %v, then %v", firstDecided, got.DecidedAt)
	}
	if _, _, found, err := s.Details(ctx, acc); err != nil || found {
		t.Errorf("SaveTriage wrote filing_details: found=%v, err=%v", found, err)
	}
}

func TestUndecided(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	f := seedFilings(t, s, 4)
	// f[0] has no label, f[1] came from older rules, f[2] from the current
	// rules (2) and f[3] from newer ones.
	saveTriage(t, s, Triage{AccessionNo: f[1].AccessionNo, Level: "low", RulesVersion: 1})
	saveTriage(t, s, Triage{AccessionNo: f[2].AccessionNo, Level: "low", RulesVersion: 2})
	saveTriage(t, s, Triage{AccessionNo: f[3].AccessionNo, Level: "low", RulesVersion: 3})

	got, err := s.Undecided(ctx, 2, 10)
	if err != nil {
		t.Fatal(err)
	}
	want := []Undecided{
		{AccessionNo: f[0].AccessionNo, FormType: f[0].FormType, CIK: f[0].CIK, FirstSeenAt: longAgo, RulesVersion: 0},
		{AccessionNo: f[1].AccessionNo, FormType: f[1].FormType, CIK: f[1].CIK, FirstSeenAt: longAgo, RulesVersion: 1},
	}
	// Test rows were seen in 1900, so oldest first puts them ahead of real ones.
	if len(got) < len(want) {
		t.Fatalf("got %d rows, want at least %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		g := got[i]
		if g.AccessionNo != w.AccessionNo || g.FormType != w.FormType || g.CIK != w.CIK ||
			!g.FirstSeenAt.Equal(w.FirstSeenAt) || g.RulesVersion != w.RulesVersion {
			t.Errorf("row %d = %+v, want %+v", i, g, w)
		}
	}
	for _, g := range got[len(want):] {
		if isTestRow(g.AccessionNo) {
			t.Errorf("%s is up to date but was listed (rules_version %d)", g.AccessionNo, g.RulesVersion)
		}
	}

	got, err = s.Undecided(ctx, 2, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].AccessionNo != f[0].AccessionNo {
		t.Errorf("limit 1: %+v, want only %s", got, f[0].AccessionNo)
	}
}

// testBacklog is TriageBacklog with the real filings' share taken out. The
// local worker may store filings meanwhile, so it retries until the count of
// real pending filings is the same before and after.
func testBacklog(t *testing.T, s *Store) Backlog {
	t.Helper()
	ctx := context.Background()
	realPending := func() int {
		var n int
		err := s.pool.QueryRow(ctx, `
			SELECT count(*) FROM filings f
			WHERE f.accession_no NOT LIKE '9999999999-%'
			  AND NOT EXISTS (SELECT 1 FROM triage t WHERE t.accession_no = f.accession_no)`).Scan(&n)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	for range 5 {
		before := realPending()
		b, err := s.TriageBacklog(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if realPending() == before {
			b.Pending -= before
			return b
		}
	}
	t.Fatal("real filings kept changing under the test")
	return Backlog{}
}

func TestTriageBacklog(t *testing.T) {
	s := testStore(t)
	f := seedFilings(t, s, 3)

	if b := testBacklog(t, s); b.Pending != 3 || !b.Oldest.Equal(longAgo) {
		t.Errorf("nothing labelled: %d test rows pending, oldest %v; want 3, %v", b.Pending, b.Oldest, longAgo)
	}

	saveTriage(t, s, Triage{AccessionNo: f[0].AccessionNo, Level: "routine", RulesVersion: 1})
	saveTriage(t, s, Triage{AccessionNo: f[1].AccessionNo, Level: "routine", RulesVersion: 1})
	if b := testBacklog(t, s); b.Pending != 1 || !b.Oldest.Equal(longAgo) {
		t.Errorf("two labelled: %d test rows pending, oldest %v; want 1, %v", b.Pending, b.Oldest, longAgo)
	}

	saveTriage(t, s, Triage{AccessionNo: f[2].AccessionNo, Level: "routine", RulesVersion: 1})
	if b := testBacklog(t, s); b.Pending != 0 || b.Oldest.Equal(longAgo) {
		t.Errorf("all labelled: %d test rows pending, oldest %v; want 0 and not %v", b.Pending, b.Oldest, longAgo)
	}
}

func TestFilingsTriage(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	// Newest first: f[1] and f[0] share a second, then f[3], f[2], then f[4].
	// Even indexes are 8-Ks, odd ones Form 4s.
	f := seedFilings(t, s, 5)
	score := 42
	saveTriage(t, s, Triage{AccessionNo: f[0].AccessionNo, Level: "high", Reasons: []string{"item 1.03"}, RulesVersion: 1})
	saveTriage(t, s, Triage{AccessionNo: f[1].AccessionNo, Level: "medium", RulesVersion: 1, ModelScore: &score, ModelReason: "big sale", Model: "test-model"})
	saveTriage(t, s, Triage{AccessionNo: f[2].AccessionNo, Level: "low", RulesVersion: 1})
	saveTriage(t, s, Triage{AccessionNo: f[3].AccessionNo, Level: "routine", RulesVersion: 1})
	// f[4] stays pending.
	if err := s.SaveDetails(ctx, f[0].AccessionNo, []string{"1.03", "9.01"}, "d.htm"); err != nil {
		t.Fatal(err)
	}

	highOrMedium := []string{"high", "medium"}
	for _, tc := range []struct {
		name string
		q    FilingQuery
		want []string
	}{
		{"levels", FilingQuery{Levels: highOrMedium, Limit: 100}, []string{f[1].AccessionNo, f[0].AccessionNo}},
		{"levels and form", FilingQuery{Levels: highOrMedium, FormType: "8-K", Limit: 100}, []string{f[0].AccessionNo}},
		{"levels and cursor", FilingQuery{Levels: highOrMedium, Limit: 100, Before: &Cursor{AcceptedAt: f[1].AcceptedAt, AccessionNo: f[1].AccessionNo}}, []string{f[0].AccessionNo}},
	} {
		rows, err := s.Filings(ctx, tc.q)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := testAccessions(rows); !slices.Equal(got, tc.want) {
			t.Errorf("%s: test rows %v, want %v", tc.name, got, tc.want)
		}
		for _, r := range rows {
			if r.Triage == nil || !slices.Contains(highOrMedium, r.Triage.Level) {
				t.Errorf("%s: let through %s with triage %+v", tc.name, r.AccessionNo, r.Triage)
			}
		}
	}

	// No level filter: everything, pending included. The five test rows are
	// the newest in the table.
	rows, err := s.Filings(ctx, FilingQuery{Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	byAcc := map[string]FilingRow{}
	for _, r := range rows {
		byAcc[r.AccessionNo] = r
	}
	if got, want := testAccessions(rows), []string{f[1].AccessionNo, f[0].AccessionNo, f[3].AccessionNo, f[2].AccessionNo, f[4].AccessionNo}; !slices.Equal(got, want) {
		t.Fatalf("no filter: test rows %v, want %v", got, want)
	}
	if tr := byAcc[f[4].AccessionNo].Triage; tr != nil {
		t.Errorf("pending filing has triage %+v", tr)
	}

	high := byAcc[f[0].AccessionNo].Triage
	if high == nil || high.Level != "high" || !slices.Equal(high.Reasons, []string{"item 1.03"}) ||
		!slices.Equal(high.Items, []string{"1.03", "9.01"}) || high.RulesVersion != 1 || high.ModelScore != nil ||
		high.AccessionNo != f[0].AccessionNo || high.DecidedAt.IsZero() {
		t.Errorf("high filing: triage %+v", high)
	}
	// No details row and no reasons: empty slices, so the API shows [] not null.
	medium := byAcc[f[1].AccessionNo].Triage
	if medium == nil || medium.Level != "medium" || medium.Items == nil || len(medium.Items) != 0 ||
		medium.Reasons == nil || len(medium.Reasons) != 0 || medium.ModelScore == nil || *medium.ModelScore != 42 ||
		medium.ModelReason != "big sale" || medium.Model != "test-model" {
		t.Errorf("medium filing: triage %+v", medium)
	}
	for acc, level := range map[string]string{f[2].AccessionNo: "low", f[3].AccessionNo: "routine"} {
		if tr := byAcc[acc].Triage; tr == nil || tr.Level != level {
			t.Errorf("%s: triage %+v, want level %s", acc, tr, level)
		}
	}
}
