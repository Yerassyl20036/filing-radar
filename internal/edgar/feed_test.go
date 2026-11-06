package edgar

import (
	"os"
	"strings"
	"testing"
	"time"
)

// testdata/latest.atom is a real page of the feed, fetched on 2026-11-06.
func fixture(t *testing.T) Feed {
	t.Helper()
	f, err := os.Open("testdata/latest.atom")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	feed, err := ParseFeed(f)
	if err != nil {
		t.Fatal(err)
	}
	return feed
}

func TestParseFeed(t *testing.T) {
	feed := fixture(t)
	if len(feed.Entries) != 100 || feed.Skipped != 0 {
		t.Fatalf("got %d entries and %d skipped, want 100 and 0", len(feed.Entries), feed.Skipped)
	}

	want := Entry{
		AccessionNo: "0001193125-26-450499",
		FormType:    "4/A",
		CIK:         "0001826510",
		Name:        "Gallotta Steven E.",
		Role:        "Reporting",
		AcceptedAt:  time.Date(2026, 11, 6, 2, 53, 57, 0, time.UTC), // 21:53:57 New York time
		URL:         "https://www.sec.gov/Archives/edgar/data/1826510/000119312526450499/0001193125-26-450499-index.htm",
	}
	got := feed.Entries[0]
	if !got.AcceptedAt.Equal(want.AcceptedAt) {
		t.Errorf("AcceptedAt = %v, want %v", got.AcceptedAt, want.AcceptedAt)
	}
	got.AcceptedAt = want.AcceptedAt
	if got != want {
		t.Errorf("first entry =\n  %+v\nwant\n  %+v", got, want)
	}

	for i, e := range feed.Entries {
		if e.FormType == "" || len(e.CIK) != 10 || e.Name == "" || e.Role == "" || e.URL == "" {
			t.Errorf("entry %d is missing a field: %+v", i, e)
		}
	}
}

func TestGroup(t *testing.T) {
	filings := Group(fixture(t).Entries)
	if len(filings) != 48 {
		t.Fatalf("got %d filings, want 48: 100 entries name 48 distinct accession numbers", len(filings))
	}

	byAcc := map[string]Filing{}
	for _, f := range filings {
		byAcc[f.AccessionNo] = f
	}
	tests := []struct {
		accession, company string
		parties            int
	}{
		// Form 4/A: the insider is listed first, but the company is the Issuer.
		{"0001193125-26-450499", "BCB BANCORP INC", 2},
		// Schedule 13D/A: the fund that filed it, and the company it's about.
		{"0000919574-26-007454", "NovaBridge Biosciences", 2},
		// Two insiders and the company.
		{"0001477932-26-006732", "APEX Tech Acquisition Inc.", 3},
		// The page ends after this filing's first entry; the Issuer entry is
		// on the next page. Grouped alone, the insider stands in. This is why
		// the poller reads every page before storing anything.
		{"0001204288-26-000007", "LYONS ROBERT C", 1},
	}
	for _, tt := range tests {
		f, ok := byAcc[tt.accession]
		if !ok {
			t.Errorf("%s: not found", tt.accession)
			continue
		}
		if f.Company != tt.company || len(f.Parties) != tt.parties {
			t.Errorf("%s: company %q with %d parties, want %q with %d", tt.accession, f.Company, len(f.Parties), tt.company, tt.parties)
		}
	}

	if filings[0].AccessionNo != "0001193125-26-450499" {
		t.Errorf("order changed: first filing is %s", filings[0].AccessionNo)
	}
}

func TestParseFeedOddEntries(t *testing.T) {
	const page = `<?xml version="1.0" encoding="ISO-8859-1" ?>
<feed xmlns="http://www.w3.org/2005/Atom">
<entry>
  <title>8-K - Caf` + "\xe9" + ` - Bar (Holdings) Inc (0000000001) (Filer)</title>
  <link href="https://www.sec.gov/a"/>
  <updated>2026-07-01T16:05:00-04:00</updated>
  <category term="8-K"/>
  <id>urn:tag:sec.gov,2008:accession-number=0000000001-26-000001</id>
</entry>
<entry>
  <title>8-K - No accession number (0000000002) (Filer)</title>
  <updated>2026-07-01T16:05:00-04:00</updated>
  <category term="8-K"/>
  <id>urn:tag:sec.gov,2008:something-else</id>
</entry>
<entry>
  <title>a title in a shape nobody expected</title>
  <link href="https://www.sec.gov/c"/>
  <updated>2026-07-01T16:06:00-04:00</updated>
  <category term="10-K"/>
  <id>urn:tag:sec.gov,2008:accession-number=0000000003-26-000003</id>
</entry>
</feed>`

	feed, err := ParseFeed(strings.NewReader(page))
	if err != nil {
		t.Fatal(err)
	}
	if len(feed.Entries) != 2 || feed.Skipped != 1 {
		t.Fatalf("got %d entries and %d skipped, want 2 and 1", len(feed.Entries), feed.Skipped)
	}

	first := feed.Entries[0]
	// Latin-1 byte 0xE9 is "é"; " - " and parentheses inside the name survive.
	if first.Name != "Café - Bar (Holdings) Inc" || first.CIK != "0000000001" || first.Role != "Filer" {
		t.Errorf("first entry: %+v", first)
	}
	// Summer time: -04:00, so 16:05 in New York is 20:05 UTC.
	if want := time.Date(2026, 7, 1, 20, 5, 0, 0, time.UTC); !first.AcceptedAt.Equal(want) {
		t.Errorf("AcceptedAt = %v, want %v", first.AcceptedAt, want)
	}
	// An odd title keeps the filing; the name is the raw text.
	if odd := feed.Entries[1]; odd.FormType != "10-K" || odd.Name != "a title in a shape nobody expected" {
		t.Errorf("odd entry: %+v", odd)
	}
}

func TestParseFeedRejectsNonFeed(t *testing.T) {
	_, err := ParseFeed(strings.NewReader(`<html><body>Your Request Originates from an Undeclared Automated Tool</body></html>`))
	if err == nil {
		t.Fatal("an HTML page parsed as a feed")
	}
}

func TestGroupPicksTheCompany(t *testing.T) {
	at := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	e := func(acc, name, role string) Entry {
		return Entry{AccessionNo: acc, FormType: "4", CIK: "000000000" + acc[len(acc)-1:], Name: name, Role: role, AcceptedAt: at, URL: "u-" + name}
	}
	filings := Group([]Entry{
		e("0000000000-26-000001", "Insider", "Reporting"),
		e("0000000000-26-000001", "Company", "Issuer"),
		e("0000000000-26-000001", "Insider", "Reporting"), // the same entry again, as overlapping pages give
		e("0000000000-26-000002", "Fund", "Filed by"),
	})
	if len(filings) != 2 {
		t.Fatalf("got %d filings, want 2", len(filings))
	}
	if f := filings[0]; f.Company != "Company" || f.URL != "u-Company" || len(f.Parties) != 2 {
		t.Errorf("first filing: %+v", f)
	}
	if f := filings[1]; f.Company != "Fund" || len(f.Parties) != 1 {
		t.Errorf("second filing: %+v", f)
	}
}
