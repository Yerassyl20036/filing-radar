// Package ingest moves new filings from EDGAR into Postgres.
package ingest

import (
	"context"
	"fmt"
	"time"

	"github.com/Yerassyl20036/filing-radar/internal/edgar"
)

// Source is where filings come from: edgar.Client, or a fake in tests.
type Source interface {
	Latest(ctx context.Context, start int) (edgar.Feed, error)
}

// Store is where they go: store.Store, or a fake in tests.
type Store interface {
	KnownAccessions(ctx context.Context, accessionNos []string) (map[string]bool, error)
	InsertFilings(ctx context.Context, filings []edgar.Filing) (int, error)
}

// MaxPages bounds one poll, so a bug can't page through EDGAR forever: at
// most 5 requests a minute, against SEC's limit of 10 a second.
const MaxPages = 5

// Result is what one poll did. The worker reports it on the status page.
type Result struct {
	Pages   int           `json:"pages"`
	Entries int           `json:"entries"`
	Filings int           `json:"filings"` // distinct filings seen
	New     int           `json:"new"`     // stored for the first time
	Skipped int           `json:"skipped,omitempty"`
	Gap     bool          `json:"gap,omitempty"` // hit maxPages before reaching stored filings
	Took    time.Duration `json:"-"`
}

// Poll pages through the newest filings until it reaches one that's already
// stored, then stores everything new in one go.
//
// Storing only after paging stops matters. EDGAR lists a filing once per
// party, and those entries can straddle two pages. Storing page by page would
// save the first half, see the second half as "already stored" on the next
// page, and stop paging too early: a gap.
//
// If any page fails, nothing is stored. The next poll starts again from the
// top, so an error delays filings but never skips them.
func Poll(ctx context.Context, src Source, st Store, maxPages int) (Result, error) {
	start := time.Now()
	var (
		res     Result
		entries []edgar.Entry
		known   = map[string]bool{}
	)
	for {
		if res.Pages == maxPages {
			res.Gap = true // still no overlap: older filings may be beyond what we read
			break
		}
		feed, err := src.Latest(ctx, res.Pages*edgar.PageSize)
		if err != nil {
			return res, fmt.Errorf("page %d: %w", res.Pages+1, err)
		}
		res.Pages++
		res.Skipped += feed.Skipped
		entries = append(entries, feed.Entries...)

		found, err := st.KnownAccessions(ctx, accessions(feed.Entries))
		if err != nil {
			return res, err
		}
		for a := range found {
			known[a] = true
		}
		overlap := len(found) > 0
		lastPage := len(feed.Entries)+feed.Skipped < edgar.PageSize
		if overlap || lastPage {
			break
		}
	}

	filings := edgar.Group(entries)
	res.Entries, res.Filings = len(entries), len(filings)

	fresh := filings[:0:0]
	for _, f := range filings {
		if !known[f.AccessionNo] {
			fresh = append(fresh, f)
		}
	}
	// The insert still ignores duplicates: another poll may have stored some
	// of these since we looked.
	n, err := st.InsertFilings(ctx, fresh)
	res.New = n
	res.Took = time.Since(start)
	return res, err
}

func accessions(entries []edgar.Entry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.AccessionNo
	}
	return out
}
