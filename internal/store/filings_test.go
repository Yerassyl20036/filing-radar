package store

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/Yerassyl20036/filing-radar/internal/edgar"
)

// These tests need a real Postgres. They run when TEST_DATABASE_URL is set,
// for example against the local stack:
//
//	TEST_DATABASE_URL=postgres://radar:radar@localhost:5432/radar?sslmode=disable go test ./internal/store
//
// They only touch rows whose accession number starts with 9999999999.
func testStore(t *testing.T) *Store {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	clean := func() {
		_, _ = s.pool.Exec(context.Background(), `DELETE FROM filings WHERE accession_no LIKE '9999999999-%'`)
	}
	clean()
	t.Cleanup(func() { clean(); s.Close() })
	return s
}

func testFilings(n int, at time.Time) []edgar.Filing {
	out := make([]edgar.Filing, n)
	for i := range out {
		out[i] = edgar.Filing{
			AccessionNo: fmt.Sprintf("9999999999-26-%06d", i),
			FormType:    []string{"8-K", "4"}[i%2],
			CIK:         "0000000001",
			Company:     "Test Co",
			AcceptedAt:  at.Add(-time.Duration(i/2) * time.Second), // two filings per second, to test ties
			URL:         "https://www.sec.gov/test",
			Parties:     []edgar.Party{{CIK: "0000000001", Name: "Test Co", Role: "Filer"}},
		}
	}
	return out
}

func TestInsertFilingsIsIdempotent(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	filings := testFilings(5, time.Now())

	n, err := s.InsertFilings(ctx, filings)
	if err != nil || n != 5 {
		t.Fatalf("first insert: %d, %v; want 5", n, err)
	}
	n, err = s.InsertFilings(ctx, filings)
	if err != nil || n != 0 {
		t.Fatalf("second insert: %d, %v; want 0", n, err)
	}

	known, err := s.KnownAccessions(ctx, []string{filings[0].AccessionNo, "9999999999-26-999999"})
	if err != nil {
		t.Fatal(err)
	}
	if !known[filings[0].AccessionNo] || len(known) != 1 {
		t.Errorf("known = %v", known)
	}
}

func TestFilingsPages(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	// Far in the future, so these rows come before any real ones.
	if _, err := s.InsertFilings(ctx, testFilings(7, time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC))); err != nil {
		t.Fatal(err)
	}

	var seen []string
	var before *Cursor
	for range 3 {
		page, err := s.Filings(ctx, FilingQuery{Limit: 3, Before: before})
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range page {
			if f.AccessionNo[:10] == "9999999999" {
				seen = append(seen, f.AccessionNo)
			}
		}
		last := page[len(page)-1]
		before = &Cursor{AcceptedAt: last.AcceptedAt, AccessionNo: last.AccessionNo}
	}
	// 7 rows in pages of 3: every row exactly once, even with equal times.
	if len(seen) != 7 {
		t.Fatalf("saw %d test rows across pages, want 7: %v", len(seen), seen)
	}
	for i := 1; i < len(seen); i++ {
		if seen[i] == seen[i-1] {
			t.Fatalf("row %s appeared twice", seen[i])
		}
	}

	eightK, err := s.Filings(ctx, FilingQuery{FormType: "8-K", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range eightK {
		if f.FormType != "8-K" {
			t.Errorf("form filter let through %s", f.FormType)
		}
	}
}
