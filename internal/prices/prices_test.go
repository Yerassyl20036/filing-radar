package prices

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func ny(y int, m time.Month, d, h, min int) time.Time {
	return time.Date(y, m, d, h, min, 0, 0, NewYork)
}

func TestNextClose(t *testing.T) {
	tests := []struct {
		name string
		in   time.Time
		want time.Time
	}{
		{"weekday morning", ny(2026, 11, 2, 10, 0), ny(2026, 11, 2, 16, 0)},
		{"weekday after the close", ny(2026, 11, 2, 16, 30), ny(2026, 11, 3, 16, 0)},
		{"exactly at the close", ny(2026, 11, 2, 16, 0), ny(2026, 11, 3, 16, 0)},
		{"given in UTC", time.Date(2026, 11, 2, 20, 30, 0, 0, time.UTC), ny(2026, 11, 2, 16, 0)},
		{"Friday evening", ny(2026, 11, 6, 18, 0), ny(2026, 11, 9, 16, 0)},
		{"Saturday", ny(2026, 11, 7, 9, 0), ny(2026, 11, 9, 16, 0)},
		{"Sunday night", ny(2026, 11, 8, 23, 59), ny(2026, 11, 9, 16, 0)},
		// Clocks fall back on Sunday 1 November 2026: the close moves from
		// 20:00 to 21:00 UTC.
		{"over the autumn DST change", ny(2026, 10, 30, 17, 0), ny(2026, 11, 2, 16, 0)},
		// And spring forward on Sunday 8 March 2026: 21:00 to 20:00 UTC.
		{"over the spring DST change", ny(2026, 3, 6, 17, 0), ny(2026, 3, 9, 16, 0)},
		// The day after Thanksgiving closes at 13:00.
		{"early close, before it", ny(2026, 11, 27, 10, 0), ny(2026, 11, 27, 13, 0)},
		{"early close, after it", ny(2026, 11, 27, 14, 30), ny(2026, 11, 30, 16, 0)},
		// Thanksgiving itself is a holiday NextClose doesn't know; the evening
		// after it still leads to Friday's early close.
		{"early close, the evening before", ny(2026, 11, 26, 17, 0), ny(2026, 11, 27, 13, 0)},
	}
	for _, tt := range tests {
		got := NextClose(tt.in)
		if !got.Equal(tt.want) {
			t.Errorf("%s: NextClose(%s) = %s, want %s", tt.name, tt.in, got, tt.want)
		}
	}
	if h := NextClose(ny(2026, 10, 30, 17, 0)).UTC().Hour(); h != 21 {
		t.Errorf("close after the autumn change is %d:00 UTC, want 21:00", h)
	}
	if h := NextClose(ny(2026, 3, 6, 17, 0)).UTC().Hour(); h != 20 {
		t.Errorf("close after the spring change is %d:00 UTC, want 20:00", h)
	}
}

func TestAround(t *testing.T) {
	closes := []Close{
		{Day: "2026-11-02", At: ny(2026, 11, 2, 16, 0), Price: 10},
		{Day: "2026-11-03", At: ny(2026, 11, 3, 16, 0), Price: 11},
		{Day: "2026-11-04", At: ny(2026, 11, 4, 16, 0), Price: 12},
	}
	tests := []struct {
		at            time.Time
		before, after string
	}{
		{ny(2026, 11, 3, 9, 0), "2026-11-02", "2026-11-03"},  // before the open: yesterday's close, then today's
		{ny(2026, 11, 3, 17, 0), "2026-11-03", "2026-11-04"}, // after the close: today's, then tomorrow's
		{ny(2026, 11, 3, 16, 0), "2026-11-02", "2026-11-04"}, // at the close: neither side uses it
		{ny(2026, 11, 4, 17, 0), "2026-11-04", ""},           // tomorrow hasn't closed yet
		{ny(2026, 11, 1, 12, 0), "", "2026-11-02"},
	}
	day := func(c *Close) string {
		if c == nil {
			return ""
		}
		return c.Day
	}
	for _, tt := range tests {
		b, a := Around(closes, tt.at)
		if day(b) != tt.before || day(a) != tt.after {
			t.Errorf("Around(%s) = %q, %q; want %q, %q", tt.at, day(b), day(a), tt.before, tt.after)
		}
	}
}

// A trimmed real answer: Monday to Wednesday, 2 to 4 November 2026, with
// Wednesday's bar still trading at the test's "now".
const chart = `{"chart":{"result":[{"meta":{"symbol":"AAPL","exchangeTimezoneName":"America/New_York"},
"timestamp":[1793629800,1793716200,1793802600],
"indicators":{"quote":[{"close":[340.5,null,345.25]}],"adjclose":[{"adjclose":[340.5,null,345.25]}]}}],"error":null}}`

func TestYahooCloses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v8/finance/chart/BRK-B" {
			t.Errorf("path %s", r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("interval") != "1d" || q.Get("period1") == "" || q.Get("period2") == "" {
			t.Errorf("query %s", r.URL.RawQuery)
		}
		if ua := r.Header.Get("User-Agent"); ua != "FilingRadar/1.0" || strings.Contains(ua, "@") {
			t.Errorf("User-Agent %q", ua)
		}
		_, _ = w.Write([]byte(chart))
	}))
	defer srv.Close()

	y := NewYahoo(srv.URL)
	y.Now = func() time.Time { return ny(2026, 11, 4, 12, 0) }
	got, err := y.Closes(context.Background(), "BRK-B", ny(2026, 10, 25, 0, 0), ny(2026, 11, 10, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	// Tuesday has no close (null) and Wednesday hasn't closed yet.
	if len(got) != 1 || got[0].Day != "2026-11-02" || got[0].Price != 340.5 || !got[0].At.Equal(ny(2026, 11, 2, 16, 0)) {
		t.Fatalf("got %+v", got)
	}

	y.Now = func() time.Time { return ny(2026, 11, 4, 16, 1) }
	got, err = y.Closes(context.Background(), "BRK-B", ny(2026, 10, 25, 0, 0), ny(2026, 11, 10, 0, 0))
	if err != nil || len(got) != 2 || got[1].Day != "2026-11-04" || got[1].Price != 345.25 {
		t.Fatalf("after the close: %+v, %v", got, err)
	}
}

// An 8-K at 14:30 on Friday 27 November 2026 came after that day's 13:00
// close: the close before it is Friday's and the one after is Monday's.
func TestAroundEarlyClose(t *testing.T) {
	var closes []Close
	for _, d := range []int{25, 27, 30} {
		at := closeOn(2026, 11, d)
		closes = append(closes, Close{Day: at.Format("2006-01-02"), At: at, Price: float64(d)})
	}
	if h := closes[1].At.Hour(); h != 13 {
		t.Fatalf("27 November closes at %d:00, want 13:00", h)
	}
	b, a := Around(closes, ny(2026, 11, 27, 14, 30))
	if b == nil || a == nil || b.Day != "2026-11-27" || a.Day != "2026-11-30" {
		t.Errorf("Around = %+v, %+v; want 2026-11-27 then 2026-11-30", b, a)
	}
}

func TestYahooUnknownAndEmpty(t *testing.T) {
	answers := map[string]struct {
		code int
		body string
	}{
		"/v8/finance/chart/NOPE": {404, `{"chart":{"result":null,"error":{"code":"Not Found","description":"No data found, symbol may be delisted"}}}`},
		"/v8/finance/chart/GONE": {200, `{"chart":{"result":null,"error":{"code":"Not Found","description":"No data found"}}}`},
		"/v8/finance/chart/NEW":  {200, `{"chart":{"result":[{"meta":{"symbol":"NEW"},"indicators":{"quote":[{}]}}],"error":null}}`},
		"/v8/finance/chart/BUSY": {429, `Too Many Requests`},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a := answers[r.URL.Path]
		w.WriteHeader(a.code)
		_, _ = w.Write([]byte(a.body))
	}))
	defer srv.Close()
	y := NewYahoo(srv.URL)
	y.MinGap = 0
	ctx := context.Background()
	from, to := ny(2026, 10, 25, 0, 0), ny(2026, 11, 10, 0, 0)

	for _, tk := range []string{"NOPE", "GONE"} {
		if _, err := y.Closes(ctx, tk, from, to); !errors.Is(err, ErrUnknownTicker) {
			t.Errorf("%s: err = %v, want ErrUnknownTicker", tk, err)
		}
	}
	if got, err := y.Closes(ctx, "NEW", from, to); err != nil || len(got) != 0 {
		t.Errorf("NEW: %v, %v; want no closes and no error", got, err)
	}
	var se *StatusError
	if _, err := y.Closes(ctx, "BUSY", from, to); !errors.As(err, &se) || se.Code != 429 {
		t.Errorf("BUSY: err = %v, want a 429 StatusError", err)
	}
}
