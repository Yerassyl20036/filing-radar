// Package prices gives the daily closing prices that stage 4 checks the
// model against, and the US market's close times.
package prices

import (
	"context"
	"errors"
	"time"
)

// Close is one trading day's closing price.
type Close struct {
	Day   string    // the trading day in New York, "2006-01-02"
	At    time.Time // when the market closed that day: 16:00 New York, 13:00 on an early close
	Price float64
}

// Source gives daily closes for a ticker between two moments, oldest first.
type Source interface {
	Closes(ctx context.Context, ticker string, from, to time.Time) ([]Close, error)
}

// ErrUnknownTicker means the source has no prices for this ticker at all;
// asking again won't change that.
var ErrUnknownTicker = errors.New("no prices for this ticker")

// NewYork is the US market's time zone. The worker imports time/tzdata, so
// this loads even in an image without zone files.
var NewYork = mustLoad("America/New_York")

func mustLoad(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return loc
}

// earlyCloses are NYSE's 13:00 closes (from its published holiday
// calendars). A filing at 14:00 on one of these days came after that day's
// close, so the close time must be right, not just the date. Extend the
// list each year; a missing day is treated as a 16:00 close.
var earlyCloses = map[string]bool{
	"2024-07-03": true, "2024-11-29": true, "2024-12-24": true,
	"2025-07-03": true, "2025-11-28": true, "2025-12-24": true,
	"2026-11-27": true, "2026-12-24": true,
	"2027-11-26": true,
}

// closeOn is the close in New York on the given day: 16:00, or 13:00 on an
// early close. The day may overflow (d+1), as time.Date allows.
func closeOn(y int, m time.Month, d int) time.Time {
	c := time.Date(y, m, d, 16, 0, 0, 0, NewYork)
	if earlyCloses[c.Format("2006-01-02")] {
		c = time.Date(c.Year(), c.Month(), c.Day(), 13, 0, 0, 0, NewYork)
	}
	return c
}

// NextClose is the first regular close strictly after t: the close on t's
// day if t is a weekday before it, otherwise on the next weekday.
// Holidays aren't known here; a close that never happened simply has no
// price, and the caller waits for the next one.
func NextClose(t time.Time) time.Time {
	ny := t.In(NewYork)
	c := closeOn(ny.Year(), ny.Month(), ny.Day())
	if !c.After(t) {
		c = closeOn(ny.Year(), ny.Month(), ny.Day()+1)
	}
	for c.Weekday() == time.Saturday || c.Weekday() == time.Sunday {
		c = closeOn(c.Year(), c.Month(), c.Day()+1)
	}
	return c
}

// Around picks the last close strictly before t and the first strictly
// after it. closes must be oldest first. Either is nil when missing.
func Around(closes []Close, t time.Time) (before, after *Close) {
	for i := range closes {
		c := &closes[i]
		switch {
		case c.At.Before(t):
			before = c
		case c.At.After(t) && after == nil:
			after = c
		}
	}
	return before, after
}
