package store

import (
	"context"
	"slices"
	"testing"
)

func saveNote(t *testing.T, s *Store, n Note) {
	t.Helper()
	if err := s.SaveNote(context.Background(), n); err != nil {
		t.Fatal(err)
	}
}

func saveDetails(t *testing.T, s *Store, acc string, items []string, primaryDoc string) {
	t.Helper()
	if err := s.SaveDetails(context.Background(), acc, items, primaryDoc); err != nil {
		t.Fatal(err)
	}
}

func TestSaveNoteOverwrites(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	acc := seedFilings(t, s, 1)[0].AccessionNo

	// Nil claims must reach the NOT NULL jsonb column as [], not NULL or null.
	saveNote(t, s, Note{AccessionNo: acc, Status: "rejected", Reason: "1 checked claim", Model: "m1", PromptVersion: 1})
	var raw string
	if err := s.pool.QueryRow(ctx, `SELECT claims::text FROM notes WHERE accession_no = $1`, acc).Scan(&raw); err != nil || raw != "[]" {
		t.Fatalf("claims after nil: %q, %v; want []", raw, err)
	}

	claims := []Claim{
		{Text: "Revenue fell.", Quote: "revenue decreased 12%", Source: "current", URL: "https://www.sec.gov/a.htm"},
		{Text: "Last time it rose.", Quote: "revenue increased", Source: "previous", URL: "https://www.sec.gov/b.htm"},
	}
	saveNote(t, s, Note{AccessionNo: acc, Status: "published", Headline: "Revenue turns down", Claims: claims,
		PreviousAccessionNo: "0000000001-25-000001", Model: "m2", PromptVersion: 2})

	var (
		n                   int
		status, headline    string
		previous, reason, m string
		version             int
	)
	err := s.pool.QueryRow(ctx, `
		SELECT count(*) OVER (), status, headline, previous_accession_no, reason, model, prompt_version
		FROM notes WHERE accession_no = $1`, acc).
		Scan(&n, &status, &headline, &previous, &reason, &m, &version)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 || status != "published" || headline != "Revenue turns down" || previous != "0000000001-25-000001" ||
		reason != "" || m != "m2" || version != 2 {
		t.Errorf("after overwrite: rows=%d status=%q headline=%q previous=%q reason=%q model=%q version=%d",
			n, status, headline, previous, reason, m, version)
	}

	rows, err := s.Filings(ctx, FilingQuery{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].AccessionNo != acc || rows[0].Note == nil {
		t.Fatalf("Filings: %+v; want %s with a note", rows, acc)
	}
	got := rows[0].Note
	if got.AccessionNo != acc || got.Status != "published" || got.Headline != "Revenue turns down" ||
		!slices.Equal(got.Claims, claims) || got.Model != "m2" || got.PromptVersion != 2 || got.CreatedAt.IsZero() {
		t.Errorf("note from Filings: %+v", got)
	}
}

func TestFilingsShowsOnlyPublishedNotes(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	// Newest first: f[1] and f[0] share a second, then f[2].
	f := seedFilings(t, s, 3)
	saveNote(t, s, Note{AccessionNo: f[0].AccessionNo, Status: "published", Headline: "no claims stored"})
	saveNote(t, s, Note{AccessionNo: f[1].AccessionNo, Status: "rejected", Reason: "too few claims"})
	// f[2] has no note.

	rows, err := s.Filings(ctx, FilingQuery{Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	byAcc := map[string]FilingRow{}
	for _, r := range rows {
		byAcc[r.AccessionNo] = r
	}
	if len(byAcc) != 3 {
		t.Fatalf("Filings returned %v; want the 3 test rows", testAccessions(rows))
	}
	if n := byAcc[f[0].AccessionNo].Note; n == nil || n.Headline != "no claims stored" || n.Claims == nil || len(n.Claims) != 0 {
		t.Errorf("published note: %+v; want headline and empty (not nil) claims", n)
	}
	for _, acc := range []string{f[1].AccessionNo, f[2].AccessionNo} {
		if n := byAcc[acc].Note; n != nil {
			t.Errorf("%s: note %+v; want nil", acc, n)
		}
	}
}

func TestNotesToWrite(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	// Even indexes are 8-Ks, odd ones Form 4s; lower indexes are newer.
	f := seedFilings(t, s, 11)
	if _, err := s.pool.Exec(ctx, `UPDATE filings SET form_type = '8-K/A' WHERE accession_no = $1`, f[6].AccessionNo); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		i      int
		level  string
		doc    string // "" with details=false means no filing_details row
		detail bool
		note   bool
	}{
		{0, "high", "a.htm", true, false},   // wanted
		{1, "high", "b.htm", true, false},   // Form 4
		{2, "high", "", true, false},        // no primary doc
		{4, "high", "c.htm", true, true},    // already has a note
		{6, "high", "d.htm", true, false},   // wanted: 8-K/A
		{8, "medium", "e.htm", true, false}, // not high
		{10, "high", "", false, false},      // no details row
	} {
		acc := f[tc.i].AccessionNo
		saveTriage(t, s, Triage{AccessionNo: acc, Level: tc.level, RulesVersion: 1})
		if tc.detail {
			saveDetails(t, s, acc, []string{"2.02"}, tc.doc)
		}
		if tc.note {
			saveNote(t, s, Note{AccessionNo: acc, Status: "rejected", Reason: "test"})
		}
	}

	tasks, err := s.NotesToWrite(ctx, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var got []NoteTask
	for _, task := range tasks {
		if isTestRow(task.AccessionNo) {
			got = append(got, task)
		}
	}
	if len(got) != 2 || got[0].AccessionNo != f[0].AccessionNo || got[1].AccessionNo != f[6].AccessionNo {
		t.Fatalf("test tasks %+v; want %s then %s", got, f[0].AccessionNo, f[6].AccessionNo)
	}
	first := got[0]
	if first.FormType != "8-K" || first.CIK != f[0].CIK || first.Company != f[0].Company || first.PrimaryDoc != "a.htm" ||
		!slices.Equal(first.Items, []string{"2.02"}) || !first.AcceptedAt.Equal(f[0].AcceptedAt) {
		t.Errorf("first task: %+v", first)
	}
	if got[1].FormType != "8-K/A" {
		t.Errorf("second task form %q; want 8-K/A", got[1].FormType)
	}

	// Test rows are the newest high 8-Ks, so limit 1 returns f[0].
	one, err := s.NotesToWrite(ctx, 1)
	if err != nil || len(one) != 1 || one[0].AccessionNo != f[0].AccessionNo {
		t.Errorf("limit 1: %+v, %v", one, err)
	}
}

func TestNotesToday(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	f := seedFilings(t, s, 2)

	before, err := s.NotesToday(ctx)
	if err != nil {
		t.Fatal(err)
	}
	saveNote(t, s, Note{AccessionNo: f[0].AccessionNo, Status: "published"})
	saveNote(t, s, Note{AccessionNo: f[1].AccessionNo, Status: "rejected"})
	// Re-saving is the same row, so it doesn't count twice.
	saveNote(t, s, Note{AccessionNo: f[1].AccessionNo, Status: "rejected", Reason: "again"})
	// A note from yesterday doesn't count.
	if _, err := s.pool.Exec(ctx, `UPDATE notes SET created_at = now() - interval '2 days' WHERE accession_no = $1`, f[1].AccessionNo); err != nil {
		t.Fatal(err)
	}

	after, err := s.NotesToday(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after-before != 1 {
		t.Errorf("NotesToday went from %d to %d; want +1", before, after)
	}
}
