package ingest

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Yerassyl20036/filing-radar/internal/edgar"
)

// fakeSource serves a fixed list of entries in pages of edgar.PageSize, like
// EDGAR at one moment in time.
type fakeSource struct {
	entries []edgar.Entry
	failAt  int // page index that fails; -1 for none
	calls   int
}

func (f *fakeSource) Latest(_ context.Context, start int) (edgar.Feed, error) {
	f.calls++
	if start/edgar.PageSize == f.failAt {
		return edgar.Feed{}, errors.New("SEC answered 503")
	}
	end := min(start+edgar.PageSize, len(f.entries))
	if start >= end {
		return edgar.Feed{}, nil
	}
	return edgar.Feed{Entries: f.entries[start:end]}, nil
}

type fakeStore struct{ rows map[string]edgar.Filing }

func newStore(accessions ...string) *fakeStore {
	s := &fakeStore{rows: map[string]edgar.Filing{}}
	for _, a := range accessions {
		s.rows[a] = edgar.Filing{AccessionNo: a}
	}
	return s
}

func (s *fakeStore) KnownAccessions(_ context.Context, nos []string) (map[string]bool, error) {
	known := map[string]bool{}
	for _, a := range nos {
		if _, ok := s.rows[a]; ok {
			known[a] = true
		}
	}
	return known, nil
}

func (s *fakeStore) InsertFilings(_ context.Context, filings []edgar.Filing) (int, error) {
	n := 0
	for _, f := range filings {
		if _, ok := s.rows[f.AccessionNo]; !ok {
			s.rows[f.AccessionNo] = f
			n++
		}
	}
	return n, nil
}

func acc(i int) string { return fmt.Sprintf("0000000000-26-%06d", i) }

// filings makes entries for accession numbers from..to-1, newest first, one
// entry each (an 8-K has a single party).
func filings(from, to int) []edgar.Entry {
	var out []edgar.Entry
	for i := to - 1; i >= from; i-- {
		out = append(out, edgar.Entry{AccessionNo: acc(i), FormType: "8-K", Name: "Co", Role: "Filer", AcceptedAt: time.Unix(int64(i), 0)})
	}
	return out
}

func upTo(n int) []string {
	var out []string
	for i := 0; i <= n; i++ {
		out = append(out, acc(i))
	}
	return out
}

func TestPoll(t *testing.T) {
	tests := []struct {
		name      string
		feed      []edgar.Entry
		storedTo  int // filings 0..storedTo are stored already; -1 for none
		failAt    int
		wantPages int
		wantNew   int
		wantGap   bool
		wantErr   bool
	}{
		{name: "quiet minute: 3 new on the first page",
			feed: filings(0, 300), storedTo: 296, failAt: -1,
			wantPages: 1, wantNew: 3},
		{name: "burst: 150 new, so a second page",
			feed: filings(0, 300), storedTo: 149, failAt: -1,
			wantPages: 2, wantNew: 150},
		{name: "first run: nothing stored, so read up to the cap",
			feed: filings(0, 1000), storedTo: -1, failAt: -1,
			wantPages: MaxPages, wantNew: MaxPages * edgar.PageSize, wantGap: true},
		{name: "end of the feed before the cap",
			feed: filings(0, 120), storedTo: -1, failAt: -1,
			wantPages: 2, wantNew: 120},
		{name: "second page fails: store nothing, retry next minute",
			feed: filings(0, 300), storedTo: 149, failAt: 1,
			wantPages: 1, wantNew: 0, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := &fakeSource{entries: tt.feed, failAt: tt.failAt}
			st := newStore(upTo(tt.storedTo)...)
			before := len(st.rows)

			res, err := Poll(context.Background(), src, st, MaxPages)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if res.Pages != tt.wantPages || res.New != tt.wantNew || res.Gap != tt.wantGap {
				t.Errorf("pages %d, new %d, gap %v; want %d, %d, %v", res.Pages, res.New, res.Gap, tt.wantPages, tt.wantNew, tt.wantGap)
			}
			if got := len(st.rows) - before; got != tt.wantNew {
				t.Errorf("store gained %d rows, want %d", got, tt.wantNew)
			}
		})
	}
}

// A Form 4 whose two entries straddle the page boundary. Storing page by page
// would save the insider as the company, then take the Issuer entry on page 2
// for "already stored" and stop: the 50 filings below it would be lost.
func TestPollFilingAcrossPages(t *testing.T) {
	split := acc(5000)
	feed := filings(1000, 1099) // 99 single-entry filings fill page 1 up to its last slot
	feed = append(feed,
		edgar.Entry{AccessionNo: split, FormType: "4", Name: "Insider", Role: "Reporting"}, // entry 100: end of page 1
		edgar.Entry{AccessionNo: split, FormType: "4", Name: "Company", Role: "Issuer"},    // entry 101: start of page 2
	)
	feed = append(feed, filings(950, 1000)...) // 50 more new filings on page 2
	feed = append(feed, filings(900, 950)...)  // then ones we already have
	stored := make([]string, 0, 50)
	for i := 900; i < 950; i++ {
		stored = append(stored, acc(i))
	}

	st := newStore(stored...)
	res, err := Poll(context.Background(), &fakeSource{entries: feed, failAt: -1}, st, MaxPages)
	if err != nil {
		t.Fatal(err)
	}
	if res.Pages != 2 || res.New != 99+1+50 {
		t.Fatalf("pages %d, new %d; want 2 and 150", res.Pages, res.New)
	}
	got := st.rows[split]
	if got.Company != "Company" || len(got.Parties) != 2 {
		t.Errorf("split filing stored as %q with %d parties, want the Issuer with 2", got.Company, len(got.Parties))
	}
}
