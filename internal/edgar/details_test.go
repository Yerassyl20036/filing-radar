package edgar

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
)

// A trimmed company JSON in SEC's shape: parallel arrays, one position per filing.
const smallSubmissions = `{
  "cik": "320193", "name": "Apple Inc.",
  "filings": {"recent": {
    "accessionNumber": ["0000320193-26-000101", "0000320193-26-000100", "0001140361-26-000042"],
    "form":            ["8-K", "10-Q", "4"],
    "items":           ["2.02,9.01", "", ""],
    "primaryDocument": ["aapl-20261030.htm", "aapl-20260927.htm", "xslF345X05/form4.xml"],
    "filingDate":      ["2026-10-30", "2026-10-31", "2026-11-01"]
  }, "files": []}
}`

func TestSubmissionsFind(t *testing.T) {
	s, err := ParseSubmissions(strings.NewReader(smallSubmissions))
	if err != nil {
		t.Fatal(err)
	}
	d, ok := s.Find("0000320193-26-000101")
	if !ok || !slices.Equal(d.Items, []string{"2.02", "9.01"}) || d.PrimaryDoc != "aapl-20261030.htm" {
		t.Errorf("8-K: %+v, %v", d, ok)
	}
	if d, ok := s.Find("0000320193-26-000100"); !ok || len(d.Items) != 0 {
		t.Errorf("10-Q should have no items: %+v, %v", d, ok)
	}
	if _, ok := s.Find("0000000000-26-000000"); ok {
		t.Error("found a filing that isn't there")
	}
}

func TestSubmissionsRejectsRaggedArrays(t *testing.T) {
	bad := strings.Replace(smallSubmissions, `"items":           ["2.02,9.01", "", ""]`, `"items": ["2.02,9.01"]`, 1)
	if _, err := ParseSubmissions(strings.NewReader(bad)); err == nil {
		t.Fatal("arrays of different lengths were accepted")
	}
}

// The real thing, saved from data.sec.gov. Every 8-K in it must have item
// numbers, and nothing else may.
func TestSubmissionsFixture(t *testing.T) {
	f, err := os.Open("testdata/submissions-0000320193.json")
	if errors.Is(err, os.ErrNotExist) {
		t.Skip("testdata/submissions-0000320193.json not saved yet")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	s, err := ParseSubmissions(f)
	if err != nil {
		t.Fatal(err)
	}
	rec := s.Filings.Recent
	if len(rec.AccessionNumber) < 100 {
		t.Fatalf("only %d recent filings", len(rec.AccessionNumber))
	}
	eightKs := 0
	for i, form := range rec.Form {
		d, ok := s.Find(rec.AccessionNumber[i])
		if !ok || d.PrimaryDoc == "" {
			t.Errorf("filing %d (%s) has no primary document", i, rec.AccessionNumber[i])
		}
		switch {
		case form == "8-K" || form == "8-K/A":
			eightKs++
			if len(d.Items) == 0 {
				t.Errorf("8-K %s has no items", rec.AccessionNumber[i])
			}
		case len(d.Items) > 0:
			t.Errorf("%s %s has items %v", form, rec.AccessionNumber[i], d.Items)
		}
	}
	if eightKs == 0 {
		t.Error("no 8-Ks in the fixture")
	}
}

func TestDetails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/submissions/CIK0000320193.json":
			_, _ = w.Write([]byte(smallSubmissions))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c, _ := NewClient(Config{DataURL: srv.URL, UserAgent: testAgent})
	ctx := context.Background()

	d, err := c.Details(ctx, "0000320193", "0000320193-26-000101")
	if err != nil || len(d.Items) != 2 {
		t.Fatalf("Details = %+v, %v", d, err)
	}
	if _, err := c.Details(ctx, "0000320193", "0000320193-26-999999"); !errors.Is(err, ErrNotListed) {
		t.Errorf("missing filing: err = %v, want ErrNotListed", err)
	}
	var se *StatusError
	if _, err := c.Details(ctx, "0000000042", "0000000042-26-000001"); !errors.As(err, &se) || se.Code != 404 {
		t.Errorf("unknown company: err = %v, want a 404", err)
	}
}

func TestDocumentURL(t *testing.T) {
	got := DocumentURL("", "0000320193", "0000320193-26-000101", "aapl-20261030.htm")
	want := "https://www.sec.gov/Archives/edgar/data/320193/000032019326000101/aapl-20261030.htm"
	if got != want {
		t.Errorf("got %s", got)
	}
}
